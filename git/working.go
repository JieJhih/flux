package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

var (
	ErrReadOnly = errors.New("cannot make a working clone of a read-only git repo")
)

// Config holds some values we use when working in the working clone of
// a repo.
type Config struct {
	Branch      string   // branch we're syncing to
	Paths       []string // paths within the repo containing files we care about
	NotesRef    string
	UserName    string
	UserEmail   string
	SigningKey  string
	SetAuthor   bool
	SkipMessage string
	GitSecret   bool
}

// Checkout is a local working clone of the remote repo. It is
// intended to be used for one-off "transactions", e.g,. committing
// changes then pushing upstream. It has no locking.
type Checkout struct {
	*Export
	config       Config
	upstream     Remote
	realNotesRef string // cache the notes ref, since we use it to push as well
}

type Commit struct {
	Signature Signature
	Revision  string
	Message   string
}

// CommitAction is a struct holding commit information
type CommitAction struct {
	Author     string
	Message    string
	SigningKey string
}

// TagAction is a struct holding tag parameters
type TagAction struct {
	Tag        string
	Revision   string
	Message    string
	SigningKey string
}

// Clone returns a local working clone of the sync'ed `*Repo`, using
// the config given.
func (r *Repo) Clone(ctx context.Context, conf Config) (*Checkout, error) {
	upstream := r.Origin()
	repoDir, err := r.workingClone(ctx, conf.Branch)
	if err != nil {
		return nil, err
	}

	if err := config(ctx, repoDir, conf.UserName, conf.UserEmail); err != nil {
		os.RemoveAll(repoDir)
		return nil, err
	}

	// We'll need the notes ref for pushing it, so make sure we have
	// it. This assumes we're syncing it (otherwise we'll likely get conflicts)
	realNotesRef, err := getNotesRef(ctx, repoDir, conf.NotesRef)
	if err != nil {
		os.RemoveAll(repoDir)
		return nil, err
	}

	r.mu.RLock()
	// Here is where we mimic `git fetch --tags --force`, but
	// _without_ overwriting head refs. This is only required for a
	// `Checkout` and _not_ for `Repo` as (bare) mirrors will happily
	// accept any ref changes to tags.
	//
	// NB: do this before any other fetch actions, as otherwise we may
	// get an 'existing tag clobber' error back.
	if err := fetch(ctx, repoDir, r.dir, `'+refs/tags/*:refs/tags/*'`); err != nil {
		os.RemoveAll(repoDir)
		r.mu.RUnlock()
		return nil, err
	}
	if err := fetch(ctx, repoDir, r.dir, realNotesRef+":"+realNotesRef); err != nil {
		os.RemoveAll(repoDir)
		r.mu.RUnlock()
		return nil, err
	}
	r.mu.RUnlock()

	if conf.GitSecret {
		if err := secretUnseal(ctx, repoDir); err != nil {
			return nil, err
		}
	}

	return &Checkout{
		Export:       &Export{dir: repoDir},
		upstream:     upstream,
		realNotesRef: realNotesRef,
		config:       conf,
	}, nil
}

// AbsolutePaths returns the absolute paths as configured. It ensures
// that at least one path is returned, so that it can be used with
// `Manifest.LoadManifests`.
func (c *Checkout) AbsolutePaths() []string {
	if len(c.config.Paths) == 0 {
		return []string{c.Dir()}
	}

	paths := make([]string, len(c.config.Paths), len(c.config.Paths))
	for i, p := range c.config.Paths {
		paths[i] = filepath.Join(c.Dir(), p)
	}
	return paths
}

// CommitAndPush commits changes made in this checkout, along with any
// extra data as a note, and pushes the commit and note to the remote repo.
func (c *Checkout) CommitAndPush(ctx context.Context, commitAction CommitAction, note interface{}, addUntracked bool) error {
	if addUntracked {
		if err := add(ctx, c.Dir(), "."); err != nil {
			return err
		}
	}

	if !check(ctx, c.Dir(), c.config.Paths, addUntracked) {
		return ErrNoChanges
	}

	commitAction.Message += c.config.SkipMessage
	if commitAction.SigningKey == "" {
		commitAction.SigningKey = c.config.SigningKey
	}

	if err := commit(ctx, c.Dir(), commitAction); err != nil {
		return err
	}

	if note != nil {
		rev, err := c.HeadRevision(ctx)
		if err != nil {
			return err
		}
		if err := addNote(ctx, c.Dir(), rev, c.config.NotesRef, note); err != nil {
			return err
		}
	}

	refs := []string{c.config.Branch}
	ok, err := refExists(ctx, c.Dir(), c.realNotesRef)
	if ok {
		refs = append(refs, c.realNotesRef)
	} else if err != nil {
		return err
	}

	if err := push(ctx, c.Dir(), c.upstream.URL, refs); err != nil {
		return PushError(c.upstream.URL, err)
	}
	return nil
}

// GetNote gets a note for the revision specified, or nil if there is no such note.
func (c *Checkout) GetNote(ctx context.Context, rev string, note interface{}) (bool, error) {
	return getNote(ctx, c.Dir(), c.realNotesRef, rev, note)
}

func (c *Checkout) HeadRevision(ctx context.Context) (string, error) {
	return refRevision(ctx, c.Dir(), "HEAD")
}

func (c *Checkout) MoveTagAndPush(ctx context.Context, tagAction TagAction) error {
	if tagAction.SigningKey == "" {
		tagAction.SigningKey = c.config.SigningKey
	}
	return moveTagAndPush(ctx, c.Dir(), c.upstream.URL, tagAction)
}

// ChangedFiles does a git diff listing changed files
func (c *Checkout) ChangedFiles(ctx context.Context, ref string) ([]string, error) {
	list, err := changed(ctx, c.Dir(), ref, c.config.Paths)
	if err == nil {
		for i, file := range list {
			list[i] = filepath.Join(c.Dir(), file)
		}
	}
	return list, err
}

func (c *Checkout) NoteRevList(ctx context.Context) (map[string]struct{}, error) {
	return noteRevList(ctx, c.Dir(), c.realNotesRef)
}

func (c *Checkout) Checkout(ctx context.Context, rev string) error {
	return checkout(ctx, c.Dir(), rev)
}

func (c *Checkout) Add(ctx context.Context, path string) error {
	return add(ctx, c.Dir(), path)
}
