package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/kenfold/kenfold/internal/oauth"
)

// oauthCmd runs `kenfold oauth ...`: the owner password and the clients the
// owner approved.
func (c *cli) oauthCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: kenfold oauth password | clients [--all] | revoke <grant-id>")
	}
	switch args[0] {
	case "password":
		return c.oauthPassword(ctx, args[1:])
	case "clients":
		return c.oauthClients(ctx, args[1:])
	case "revoke":
		return c.oauthRevoke(ctx, args[1:])
	default:
		return usageErr("unknown oauth subcommand %q (password, clients, revoke)", args[0])
	}
}

// oauthPassword sets the owner password. It prompts on a terminal (twice,
// without echo); otherwise it reads one line from stdin, so it can be run
// as `docker compose exec -T kenfold kenfold oauth password < file`.
func (c *cli) oauthPassword(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageErr("password takes no arguments (it reads the password from the terminal or stdin)")
	}
	pw, err := readPassword(c.in, c.errOut)
	if err != nil {
		return err
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.oauthDB.SetOwnerPassword(ctx, pw); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "Owner password set. It logs you in to the dashboard (/dashboard/) and approves OAuth clients; open dashboard sessions have ended.")
	return nil
}

func readPassword(in io.Reader, prompt io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(prompt, "Owner password (at least %d characters): ", oauth.MinPasswordRunes)
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		fmt.Fprint(prompt, "Repeat: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the passwords do not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("no password on stdin")
	}
	return pw, nil
}

func (c *cli) oauthClients(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("oauth clients", flag.ContinueOnError)
	all := fs.Bool("all", false, "include revoked grants")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("oauth clients takes flags only")
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	gs, err := rt.oauthDB.Grants(ctx, *all)
	if err != nil {
		return err
	}
	if len(gs) == 0 {
		fmt.Fprintln(c.errOut, "No OAuth clients have been approved.")
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "GRANT\tCLIENT\tAGENT\tACCESS\tAPPROVED\tLAST USED\tREVOKED")
	for _, g := range gs {
		access := "read"
		if len(g.Scopes) > 1 {
			access = "read+write"
		}
		name := g.ClientName
		if name == "" {
			name = g.ClientID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, oneLine(name, 40), g.Agent, access, ts(&g.CreatedAt), ts(g.LastUsedAt), ts(g.RevokedAt))
	}
	return w.Flush()
}

func (c *cli) oauthRevoke(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: kenfold oauth revoke <grant-id>")
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	g, err := rt.oauthDB.RevokeGrant(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Revoked grant %s (%s, agent %s) and its tokens.\n", g.ID, g.ClientID, g.Agent)
	return nil
}
