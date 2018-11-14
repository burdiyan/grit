// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Package git implements support for querying and patching
// git repositories. Operations in this package are intended
// to be used in command line tooling and are therefore
// generally fatal on error.
package git

import (
	"bytes"
	"crypto"
	_ "crypto/sha1"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/log"
)

// Dir is the directory in which git checkouts are made.
var Dir = "/var/tmp/grit"

var digester = digest.Digester(crypto.SHA1)

const gitTimeLayout = "Mon, 2 Jan 2006 15:04:05 -0700"

// A Repo is a cached git repository against which
// supported git operations are issued.
type Repo struct {
	url    string
	branch string
	root   string
	prefix string
}

// Open returns a repo representing the provided git remote url, branch, and
// prefix within the repository. The prefix is interpreted to provide
// a "view" into the git repository: all operations apply only to
// this prefix. Repositories are not safe for concurrent operations
// even across multiple uses on the same machine.
func Open(url, prefix, branch string) *Repo {
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	h := sha256.New()
	h.Write([]byte(url))
	b := h.Sum(nil)
	path := filepath.Join(Dir, fmt.Sprintf("%s%02x%02x%02x%02x", base, b[0], b[1], b[2], b[3]))
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("stat %s: %v", path, err)
	}
	r := &Repo{url: url, root: path, prefix: prefix, branch: branch}
	if err != nil {
		os.MkdirAll(path, 0777)
		r.git(nil, "clone", "--single-branch", r.url, r.root)
	}
	r.git(nil, "fetch", "origin", branch)
	r.git(nil, "reset", "--hard", "FETCH_HEAD")
	return r
}

// Log returns a set of commit objects representing the "git log" operation
// with the provided arguments.
func (r *Repo) Log(args ...string) (commits []*Commit) {
	args = append([]string{"log"}, args...)
	if r.prefix != "" {
		args = append(args, r.prefix)
	}
	foreach(r.git(nil, args...), "commit", func(commit []byte) error {
		c := &Commit{repo: r}
		headers := scan(&commit, "\n")
		digest := scanLine(&headers)
		digest = bytes.TrimPrefix(digest, []byte("commit "))
		var err error
		c.Digest, err = digester.Parse(string(digest))
		if err != nil {
			log.Fatalf("invalid commit digest %v: %v", digest, err)
		}
		for headers != nil {
			line := scanLine(&headers)
			keyval := strings.SplitN(string(line), ":", 2)
			key, val := keyval[0], keyval[1]
			val = strings.TrimLeftFunc(val, unicode.IsSpace)
			c.Headers = append(c.Headers, Header{key, val})
		}
		commit = bytes.TrimPrefix(commit, []byte("    "))
		c.Body = string(bytes.Replace(commit, []byte("\n    "), []byte("\n"), -1))
		commits = append(commits, c)
		return nil
	})
	return
}

var (
	prefixA = []byte("--- a/")
	prefixB = []byte("+++ b/")
)

// Patch returns a patch representing the commit named by the
// provided ID.
func (r *Repo) Patch(id digest.Digest) Patch {
	raw := r.git(nil, "format-patch", "-1", id.Hex(), "--stdout")
	patch, err := parsePatch(raw)
	if err != nil {
		log.Fatalf("parse patch %v: %v", id, err)
	}
	var diffs []Diff
	for _, diff := range patch.Diffs {
		if strings.HasPrefix(diff.Path, r.prefix) {
			diff.Path = strings.TrimPrefix(diff.Path, r.prefix)
			// Also rewrite any --- or +++ meta lines that begin with a/ or b/,
			// since they are also paths. The rest of meta is opaque to us.
			meta := diff.Meta
			diff.Meta = nil
			for meta != nil {
				line := scanLine(&meta)
				switch {
				case bytes.HasPrefix(line, prefixA) || bytes.HasPrefix(line, prefixB):
					path := bytes.TrimPrefix(line[len(prefixA):], []byte(r.prefix))
					diff.Meta = append(diff.Meta, line[:len(prefixA)]...)
					diff.Meta = append(diff.Meta, path...)
					diff.Meta = append(diff.Meta, '\n')
				default:
					diff.Meta = append(diff.Meta, line...)
					diff.Meta = append(diff.Meta, '\n')
				}
			}
			diff.Meta = bytes.TrimSuffix(diff.Meta, []byte{'\n'})
			diffs = append(diffs, diff)
		} else {
			log.Debug.Printf("dropping diff with path %s not in prefix %s", diff.Path, r.prefix)
		}
	}
	patch.Diffs = diffs
	return patch
}

// Apply applies a patch to the repository.
func (r *Repo) Apply(patch Patch) {
	var b bytes.Buffer
	if err := patch.Write(&b); err != nil {
		log.Fatalf("patch write: %v", err)
	}
	log.Debug.Printf("applying patch %s", patch.ID.Hex()[:7])
	r.git(b.Bytes(), "am", "--keep-non-patch", "--keep-cr")
}

// Push pushes the current state of the repository to the provided
// branch on the provided remote.
func (r *Repo) Push(remote, remoteBranch string) {
	r.git(nil, "push", remote, "HEAD:"+remoteBranch)
}

func (r *Repo) path(elems ...string) string {
	return filepath.Join(append([]string{r.root}, elems...)...)
}

func (r *Repo) git(stdin []byte, arg ...string) []byte {
	cmd := exec.Command("git", append([]string{"-C", r.root}, arg...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = append(os.Environ(), "GIT_LFS_SKIP_SMUDGE=1")
	log.Debug.Printf("%s: git %s", r.root, strings.Join(arg, " "))
	if err := cmd.Run(); err != nil {
		outerr := string(stderr.Bytes())
		if len(outerr) > 0 {
			outerr = "\n" + outerr
		}
		log.Fatalf("%s: git %s: error: %v%s", r.root, strings.Join(arg, " "), err, outerr)
	}
	outerr := string(stderr.Bytes())
	if len(outerr) > 0 {
		outerr = "\n" + outerr
	}
	log.Debug.Printf("%s: git %s: ok%s", r.root, strings.Join(arg, " "), outerr)
	return stdout.Bytes()
}

// Header is a commit header.
type Header struct{ K, V string }

// Commit represents a single commit.
type Commit struct {
	// Digest is the git hash for the commit.
	Digest digest.Digest
	// Headers is the set of headers present in the commit.
	Headers []Header
	// Body is the commit message.
	Body string

	repo *Repo
}

var shipitRe = regexp.MustCompile(`(?:fb)?shipit-source-id: ([a-z0-9]+)`)

// ShipitID returns the shipit ID, if any.
func (c *Commit) ShipitID() string {
	g := shipitRe.FindStringSubmatch(c.Body)
	switch len(g) {
	case 0:
		return ""
	case 2:
		return g[1]
	default:
		log.Fatalf("invalid commit %s", c)
		panic("not reached")
	}
}

// String returns a "one-line" commit message.
func (c *Commit) String() string {
	return fmt.Sprintf("%s: %s", c.Digest.Short(), c.Title())
}

// Title returns the commit's title -- the first line of its body.
func (c *Commit) Title() string {
	return strings.SplitN(c.Body, "\n", 2)[0]
}