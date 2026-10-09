package bench

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// LoCoMoTurnIndex contains source turns only, never question labels. Sessions
// retain dataset order; scopes must correspond to the raw arm exactly.
type LoCoMoTurnIndex map[string]map[string][]Turn

type readerContext struct {
	Contents    []string
	PrimaryIDs  []string
	NeighborIDs []string
	IDs         []string // actual payload order, including primary + neighbors
}

func rawLoCoMoScope(scope string) bool {
	return strings.HasPrefix(scope, "project:bench-locomo-") && !strings.HasPrefix(scope, "project:bench-locomo-x-")
}

func effectiveAnswerOptions(q scoredQ, withF1 bool, opts AnswerOptions) AnswerOptions {
	if !withF1 || !rawLoCoMoScope(q.Scope) {
		opts.ContextPolicy = ""
	}
	return opts
}

func newLoCoMoTurnIndex(samples []LoCoMoSample) (LoCoMoTurnIndex, error) {
	index := LoCoMoTurnIndex{}
	for _, sample := range samples {
		scope, err := memory.Scope("bench-locomo-" + sample.ID)
		if err != nil {
			return nil, err
		}
		if _, exists := index[scope]; exists {
			return nil, errors.New("duplicate LoCoMo scope")
		}
		sessions := map[string][]Turn{}
		seen := map[string]bool{}
		for _, turn := range sample.Turns {
			if turn.DiaID == "" || sessionOfDia(turn.DiaID) == turn.DiaID || seen[turn.DiaID] {
				return nil, errors.New("invalid or duplicate LoCoMo turn ID")
			}
			seen[turn.DiaID] = true
			sid := sessionOfDia(turn.DiaID)
			sessions[sid] = append(sessions[sid], turn)
		}
		index[scope] = sessions
	}
	return index, nil
}

func buildLoCoMoReaderContext(scope string, hits []store.Scored, index LoCoMoTurnIndex, radius, maxBytes int) (readerContext, error) {
	var out readerContext
	if !rawLoCoMoScope(scope) || index[scope] == nil {
		return out, errors.New("raw LoCoMo context requires its frozen source index")
	}
	if radius < 0 || maxBytes <= 0 {
		return out, errors.New("invalid context radius or byte budget")
	}
	sessions := index[scope]
	selected, primary := map[string]bool{}, map[string]bool{}
	var sessionOrder []string
	used := 0
	for _, hit := range hits {
		id := attrString(hit.Memory, "bench_dia")
		sid := sessionOfDia(id)
		found := false
		for _, turn := range sessions[sid] {
			if turn.DiaID == id {
				found = hit.Scope == scope && hit.Content == clipBytes(turn.Content, maxContentBytes) && hit.CreatedAt.Equal(turn.When)
				break
			}
		}
		if !found {
			return out, fmt.Errorf("seed %s does not match frozen raw scope/source", hit.ID)
		}
		if selected[id] {
			continue
		}
		selected[id], primary[id] = true, true
		out.PrimaryIDs = append(out.PrimaryIDs, id)
		if !containsString(sessionOrder, sid) {
			sessionOrder = append(sessionOrder, sid)
		}
		used += len(hit.Content)
	}
	if used > maxBytes {
		return readerContext{}, errors.New("seed context exceeds byte budget")
	}
	// All primary turns are reserved before any optional neighbors are added.
	for _, hit := range hits {
		id := attrString(hit.Memory, "bench_dia")
		ts := sessions[sessionOfDia(id)]
		for p, turn := range ts {
			if turn.DiaID != id {
				continue
			}
			for j := max(0, p-radius); j < min(len(ts), p+radius+1); j++ {
				n := ts[j]
				content := clipBytes(n.Content, maxContentBytes)
				if selected[n.DiaID] || used+len(content) > maxBytes {
					continue
				}
				selected[n.DiaID] = true
				used += len(content)
			}
			break
		}
	}
	for _, sid := range sessionOrder {
		for _, turn := range sessions[sid] {
			if !selected[turn.DiaID] {
				continue
			}
			out.Contents = append(out.Contents, clipBytes(turn.Content, maxContentBytes))
			out.IDs = append(out.IDs, turn.DiaID)
			if !primary[turn.DiaID] {
				out.NeighborIDs = append(out.NeighborIDs, turn.DiaID)
			}
		}
	}
	return out, nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
