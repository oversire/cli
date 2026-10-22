package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/wire"
)

const digestUsage = "usage: oversire-runner digest --harness claude --session <id or file> [--markers] [--source S] [--earlier N] [--full N] [--max-chars N]"

// digest prints the SessionDigest of a session, the same as oversire/scripts/digest.py.
// With --markers, an error goes to stdout as OVERSIRE-DIGEST-ERROR and the exit code is 1.
func (c *cli) digest(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	harness := fs.String("harness", digest.Claude, "the harness of the session")
	session := fs.String("session", "", "the harness session id, or the path of the transcript file")
	markers := fs.Bool("markers", false, "print gzip and base64 between OVERSIRE-DIGEST-BEGIN and OVERSIRE-DIGEST-END")
	source := fs.String("source", wire.DigestFromFile, "the source field of the digest")
	earlier := fs.Int("earlier", wire.DefaultEarlier, "the number of earlier turns")
	full := fs.Int("full", wire.DefaultFull, "the number of last turns with the full text")
	maxChars := fs.Int("max-chars", wire.DefaultMaxChars, "the maximum length of the text of one last turn")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	if *session == "" && fs.NArg() == 1 {
		*session = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return &exitError{2, errors.New(digestUsage)}
	}
	if *session == "" || *earlier < 0 || *full < 0 || *maxChars < 1 {
		return &exitError{2, errors.New(digestUsage)}
	}

	body, err := c.readDigest(*harness, *session, wire.NewHistoryBudget(*earlier, *full, *maxChars), *source)
	if err != nil {
		if !*markers {
			return err
		}
		fmt.Fprintln(c.out, "OVERSIRE-DIGEST-ERROR "+strings.ReplaceAll(err.Error(), "\n", " "))
		return &exitError{1, errSilent}
	}
	if !*markers {
		_, err = fmt.Fprintln(c.out, string(body))
		return err
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	w.Write(body)
	w.Close()
	b := base64.StdEncoding.EncodeToString(z.Bytes())
	fmt.Fprintln(c.out, "OVERSIRE-DIGEST-BEGIN")
	for i := 0; i < len(b); i += 76 {
		fmt.Fprintln(c.out, b[i:min(i+76, len(b))])
	}
	fmt.Fprintln(c.out, "OVERSIRE-DIGEST-END")
	return nil
}

func (c *cli) readDigest(harness, session string, b wire.HistoryBudget, source string) ([]byte, error) {
	r, err := digest.ForHarness(harness, c.home)
	if err != nil {
		return nil, err
	}
	d, err := r.Digest(session, &b, source)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}
