package ticket

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxAttachment is the upload limit of a Plane attachment.
const MaxAttachment = 5 << 20

// margin covers the bytes that the gzip writer keeps before it writes them out.
const margin = 512 << 10

// Packer writes a JSONL transcript as gzip files of at most Max bytes each.
// It cuts only at line ends, so each part is valid JSONL. A single line longer than Max stays whole.
type Packer struct{ Max int64 }

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type part struct {
	f   *os.File
	c   *counter
	z   *gzip.Writer
	any bool
}

func (p *part) close() error {
	if err := p.z.Close(); err != nil {
		p.f.Close()
		return err
	}
	return p.f.Close()
}

// Pack writes src into dir and gives the paths: <name>.jsonl.gz, or <name>.partN.jsonl.gz when it needs more parts.
func (p Packer) Pack(src, dir, name string) ([]string, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	limit := max(p.Max-margin, p.Max/2)
	var tmps []string
	open := func() (*part, error) {
		f, err := os.CreateTemp(dir, name+".*.tmp")
		if err != nil {
			return nil, err
		}
		tmps = append(tmps, f.Name())
		c := &counter{w: f}
		return &part{f: f, c: c, z: gzip.NewWriter(c)}, nil
	}
	fail := func(err error) ([]string, error) {
		for _, t := range tmps {
			os.Remove(t)
		}
		return nil, err
	}
	cur, err := open()
	if err != nil {
		return fail(err)
	}
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			if cur.any && cur.c.n >= limit {
				if err := cur.close(); err != nil {
					return fail(err)
				}
				if cur, err = open(); err != nil {
					return fail(err)
				}
			}
			if _, err := cur.z.Write(line); err != nil {
				return fail(err)
			}
			cur.any = true
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(rerr)
		}
	}
	if err := cur.close(); err != nil {
		return fail(err)
	}
	out := make([]string, len(tmps))
	for i, t := range tmps {
		out[i] = filepath.Join(dir, name+".jsonl.gz")
		if len(tmps) > 1 {
			out[i] = filepath.Join(dir, fmt.Sprintf("%s.part%d.jsonl.gz", name, i+1))
		}
		if err := os.Rename(t, out[i]); err != nil {
			return fail(err)
		}
	}
	return out, nil
}
