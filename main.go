// hsync mirrors registered source directories into target directories via
// hardlinks (no content copies). Source is the single source of truth:
//   - files/dirs new in source get hardlinked / mkdir'd into target
//   - files/dirs gone from source get unlinked / removed in target
//   - same-name pairs with a broken link (atomic-save replaced the inode)
//     are re-linked from source
//
// The sync only touches first-level-and-below tree names; it never reads file
// contents. Skipped items (locked files, EXDEV, conflicts) print a warning to
// stderr and the process still exits 0, so it is safe to call from tool
// hooks after every agent file operation.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxConcurrency bounds simultaneous filesystem-work pairs. On NTFS a burst
// of hundreds of ReadDir/Link syscalls across many pairs can exhaust file
// handles or trip Defender heuristics; a small semaphore keeps that stable.
const maxConcurrency = 16

// skipName implements the hard-coded blocklist: dot-entries and node_modules
// are ignored on both sides of a sync (not linked, not mirrored, not removed).
func skipName(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules"
}

// ---------------------------------------------------------------------------
// registry

const registryName = "registry.json"

type registryFile struct {
	Version int      `json:"version"`
	Entries []entryT `json:"entries"`
}

type entryT struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
}

func registryDir() string {
	if d := os.Getenv("HSYNC_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Last resort; add/remove will surface the error via save().
		return ".hsync"
	}
	return filepath.Join(home, ".hsync")
}

func registryPath() string { return filepath.Join(registryDir(), registryName) }

// loadRegistry reads the registry. Missing file -> empty registry.
// Unreadable/corrupt file -> error (never silently recreated).
func loadRegistry() ([]entryT, error) {
	data, err := os.ReadFile(registryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read registry %s: %w", registryPath(), err)
	}
	var rf registryFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", registryPath(), err)
	}
	return rf.Entries, nil
}

// saveRegistry atomically replaces the registry file. Concurrent sync
// processes only read the registry, so a temp+rename swap is enough to keep
// them from ever observing a partial file.
func saveRegistry(entries []entryT) error {
	if err := os.MkdirAll(registryDir(), 0o700); err != nil {
		return fmt.Errorf("create registry dir: %w", err)
	}
	tmp, err := os.CreateTemp(registryDir(), registryName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp registry: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(registryFile{Version: 1, Entries: entries}); err != nil {
		tmp.Close()
		return fmt.Errorf("encode registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	if err := os.Rename(tmpName, registryPath()); err != nil {
		return fmt.Errorf("replace registry: %w", err)
	}
	return nil
}

func entryID(source, target string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + target))
	return hex.EncodeToString(sum[:4])
}

// ---------------------------------------------------------------------------
// sync

// syncStats accumulates per-run counters, shared by concurrent workers.
type syncStats struct {
	mu      sync.Mutex
	linked  int // hardlinks created
	relink  int // broken links re-created
	removed int // orphans (file or dir) removed
	issues  int // items skipped with a warning
}

func (s *syncStats) bump(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "linked":
		s.linked++
	case "relink":
		s.relink++
	case "removed":
		s.removed++
	}
}

func (s *syncStats) noteIssue() {
	s.mu.Lock()
	s.issues++
	s.mu.Unlock()
}

type syncApp struct {
	stats syncStats
	warn  func(format string, args ...any)
}

func (a *syncApp) warning(format string, args ...any) {
	a.stats.noteIssue()
	a.warn(format, args...)
}

// taskPool runs per-pair tasks on maxConcurrency workers that pull from a
// queue, instead of spawning one goroutine per entry. A pool keeps the
// goroutine count bounded and gives task control a single home: submit() can
// enqueue while workers are mid-flight, finish() is the synchronous drain
// barrier the summary needs, and later concerns (priority, retry, cancel)
// land in this queue rather than in the sync loop.
type taskPool struct {
	jobs chan entryT
	app  *syncApp
	wg   sync.WaitGroup
}

func newTaskPool(app *syncApp) *taskPool {
	return &taskPool{
		// Buffer of maxConcurrency: submit() from the main goroutine blocks
		// only when every worker is busy, never on a full pipeline.
		jobs: make(chan entryT, maxConcurrency),
		app:  app,
	}
}

// start spawns the fixed worker set. Workers idle on the queue until submit.
func (p *taskPool) start() {
	for i := 0; i < maxConcurrency; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for e := range p.jobs {
				p.app.syncEntry(e.Source, e.Target)
			}
		}()
	}
}

// submit enqueues a task. Safe to call any time after start().
func (p *taskPool) submit(e entryT) {
	p.jobs <- e
}

// finish closes the queue and blocks until every worker drains it.
func (p *taskPool) finish() {
	close(p.jobs)
	p.wg.Wait()
}

// syncEntry mirrors one registry pair, walking the tree recursively.
func (a *syncApp) syncEntry(srcRoot, dstRoot string) {
	if fi, err := os.Stat(srcRoot); err != nil || !fi.IsDir() {
		a.warning("skip %q: source not a directory: %v", srcRoot, err)
		return
	}
	if fi, err := os.Stat(dstRoot); err != nil || !fi.IsDir() {
		a.warning("skip %q -> %q: target not a directory: %v", srcRoot, dstRoot, err)
		return
	}
	a.syncDir(srcRoot, dstRoot)
}

// syncDir renders dstDir to mirror srcDir at one tree level, then recurses
// into subdirectories.
func (a *syncApp) syncDir(srcDir, dstDir string) {
	srcEntries, err := os.ReadDir(srcDir)
	if err != nil {
		a.warning("skip %s: %v", srcDir, err)
		return
	}
	dstEntries, err := os.ReadDir(dstDir)
	if err != nil {
		a.warning("skip %s: %v", dstDir, err)
		return
	}

	src := map[string]fs.DirEntry{}
	dst := map[string]fs.DirEntry{}
	for _, e := range srcEntries {
		if !skipName(e.Name()) {
			src[e.Name()] = e
		}
	}
	for _, e := range dstEntries {
		if !skipName(e.Name()) {
			dst[e.Name()] = e
		}
	}

	names := make([]string, 0, len(src)+len(dst))
	for n := range src {
		names = append(names, n)
	}
	for n := range dst {
		if _, ok := src[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, n := range names {
		srcE, srcOK := src[n]
		dstE, dstOK := dst[n]
		srcPath := filepath.Join(srcDir, n)
		dstPath := filepath.Join(dstDir, n)

		switch {
		case srcOK && dstOK:
			// Both sides have the name. Reconcile by type.
			if srcE.IsDir() && dstE.IsDir() {
				a.syncDir(srcPath, dstPath)
				continue
			}
			if srcE.IsDir() != dstE.IsDir() {
				a.warning("conflict %s: file/dir mismatch, leaving as-is", dstPath)
				continue
			}
			// Both regular files (or both symlinks; we do not mirror symlinks,
			// so a symlink pair stays untouched).
			if srcE.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if sameFile(srcPath, dstPath) {
				continue // healthy hardlink
			}
			// Broken link: target holds a stale copy from an atomic-save
			// rename. Re-link from source, which is the truth.
			if err := os.Remove(dstPath); err != nil {
				a.warning("relink %s: remove: %v", dstPath, err)
				continue
			}
			if err := os.Link(srcPath, dstPath); err != nil {
				a.warning("relink %s -> %s: %v", srcPath, dstPath, err)
				continue
			}
			a.stats.bump("relink")

		case srcOK:
			// New in source. Dir gets a real physical dir then recurses;
			// regular file gets a hardlink; symlinks are not mirrored.
			if srcE.IsDir() {
				if err := os.MkdirAll(dstPath, 0o755); err != nil {
					a.warning("mkdir %s: %v", dstPath, err)
					continue
				}
				a.syncDir(srcPath, dstPath)
				continue
			}
			if srcE.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if err := os.Link(srcPath, dstPath); err != nil {
				a.warning("link %s -> %s: %v", srcPath, dstPath, err)
				continue
			}
			a.stats.bump("linked")

		case dstOK:
			// Gone from source: target side is an orphan. Files get removed;
			// dirs get removed recursively; symlinks are left alone.
			if dstE.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if err := os.RemoveAll(dstPath); err != nil {
				a.warning("remove %s: %v", dstPath, err)
				continue
			}
			a.stats.bump("removed")
		}
	}
}

// sameFile reports whether srcPath and dstPath name the same inode, without
// reading any content.
func sameFile(srcPath, dstPath string) bool {
	si, err := os.Stat(srcPath)
	if err != nil {
		return false
	}
	di, err := os.Stat(dstPath)
	if err != nil {
		return false
	}
	return os.SameFile(si, di)
}

// ---------------------------------------------------------------------------
// commands

func cmdSync() {
	entries, err := loadRegistry()
	if err != nil {
		fatal(err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "[hsync] warning: no registered pairs (run 'hsync add <src> <dest>' first)")
		return
	}

	app := &syncApp{warn: func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "[hsync] warning: "+f+"\n", a...)
	}}
	pool := newTaskPool(app)
	pool.start()
	for _, e := range entries {
		pool.submit(e)
	}
	pool.finish()

	fmt.Printf("hsync: linked=%d relink=%d removed=%d issues=%d\n",
		app.stats.linked, app.stats.relink, app.stats.removed, app.stats.issues)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "hsync: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Print(`hsync — hardlink directory mirror

usage:
  hsync add <src> <dest>    register a pair and sync immediately (source is truth)
  hsync remove <id|src>     remove a registered pair
  hsync list                print all registered pairs
  hsync sync                sync every registered pair

registry: ~/.hsync/registry.json (override with $HSYNC_HOME)
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "add":
		err = cmdAdd(args)
	case "remove":
		err = cmdRemove(args)
	case "list":
		err = cmdList(args)
	case "sync":
		cmdSync()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hsync: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func cmdAdd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("add requires exactly <src> <dest>")
	}
	src, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("src: %w", err)
	}
	dst, err := filepath.Abs(args[1])
	if err != nil {
		return fmt.Errorf("dest: %w", err)
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("source %s is not an existing directory", src)
	}
	_, dstErr := os.Stat(dst)
	if dstErr == nil && sameFile(src, dst) {
		return fmt.Errorf("source and dest are the same directory")
	}

	entries, err := loadRegistry()
	if err != nil {
		return err
	}
	id := entryID(src, dst)
	for _, e := range entries {
		if e.ID == id {
			return fmt.Errorf("pair already registered: %s -> %s (id %s)", e.Source, e.Target, e.ID)
		}
	}
	entries = append(entries, entryT{ID: id, Source: src, Target: dst})
	if err := saveRegistry(entries); err != nil {
		return err
	}

	if dstErr != nil {
		// Target does not exist: create it explicitly so the pair can mirror.
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("create target %s: %w", dst, err)
		}
	}
	app := &syncApp{warn: func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "[hsync] warning: "+f+"\n", a...)
	}}
	app.syncEntry(src, dst)
	fmt.Printf("hsync: added %s (%s -> %s)\n", id, src, dst)
	return nil
}

func cmdRemove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("remove requires exactly <id|src>")
	}
	key := args[0]
	cleanKey := filepath.Clean(key)
	absKey, absErr := filepath.Abs(key)
	entries, err := loadRegistry()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		match := e.ID == key
		if !match && filepath.Clean(e.Source) == cleanKey {
			match = true
		}
		if !match && absErr == nil {
			if a, err := filepath.Abs(e.Source); err == nil {
				match = a == absKey
			}
		}
		if !match {
			kept = append(kept, e)
		} else {
			fmt.Printf("hsync: removed %s -> %s\n", e.Source, e.Target)
		}
	}
	if len(kept) == len(entries) {
		return fmt.Errorf("no matching pair for %q", key)
	}
	return saveRegistry(kept)
}

func cmdList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("list takes no arguments")
	}
	entries, err := loadRegistry()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("(no registered pairs)")
		return nil
	}
	for _, e := range entries {
		fmt.Printf("%-8s  %s  ->  %s\n", e.ID, e.Source, e.Target)
	}
	return nil
}
