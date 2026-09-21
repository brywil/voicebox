package main

// Server-side conversation storage with live fan-out to every connected device.
//
// WHY THE SERVER OWNS THIS AND NOT THE BROWSER. Conversations used to live only in the page,
// so a refresh lost them -- and on a phone a refresh is one mis-swipe away. localStorage was
// the obvious fix and the wrong one: it is per-browser, so it cannot follow you from phone to
// iPad to laptop, and it is not meant to hold megabytes of transcript. The server is already
// in the data path for every message (the client never talks to a model backend directly), so
// it is the only place that can both persist reliably and see enough to synchronise.
//
// WHY JSON FILES AND NOT SQLITE. go.mod has zero dependencies and that is worth keeping. SQLite
// means cgo or a large pure-Go driver; neither buys anything at the scale of one person's
// conversations. One file per session is greppable, trivially backed up, and survives this
// program being rewritten. If listing ever gets slow, add an index then -- not now.
//
// SEQUENCE NUMBERS ARE THE LOAD-BEARING PART. Every message gets a monotonic seq within its
// session, which is what makes three separate things work: a new device can ask for only the
// last N messages instead of an entire history, a reconnecting device can ask for everything
// after the last seq it saw, and a device can recognise and discard the echo of its own turn.
// Without them the client would have to diff transcripts, which is where this kind of feature
// usually goes wrong.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Seq     int64  `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      int64  `json:"ts"`
}

type Session struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	NextSeq int64  `json:"next_seq"`
	// Summary replaces every message at or below CompactedThrough when a client builds the
	// context it sends to the model. The messages themselves are NOT removed -- compaction
	// changes what is sent, never what is kept, so a bad summary can be redone from the
	// original turns and the record stays readable.
	Summary          string `json:"summary,omitempty"`
	CompactedThrough int64  `json:"compacted_through,omitempty"`
	// Backend and Model record what this conversation is being run with, so the idle sweeper
	// can summarise with the SAME model the conversation uses. The sweeper fires outside any
	// request and would otherwise have nothing to go on but the server default -- which on a
	// multi-backend setup means summarising a cloud conversation with a local model, or vice
	// versa.
	Backend  string    `json:"backend,omitempty"`
	Model    string    `json:"model,omitempty"`
	Messages []Message `json:"messages"`
}

// SessionMeta is what the picker needs: enough to choose, not the transcript.
type SessionMeta struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Count   int    `json:"count"`
}

// Event is one broadcast to the devices watching a session.
//
// Origin carries the client id that caused the change so the device that sent it can drop its
// own echo. The alternative -- having the sender render nothing and wait for the round trip --
// would be a single source of truth but would also make the sender's own reply arrive late,
// and the assistant text is already streaming to that device token by token.
type Event struct {
	Session string `json:"session"`
	Origin  string `json:"origin"`
	// Kind distinguishes what happened:
	//
	//   commit         whole turns, persisted, carrying sequence numbers
	//   compacting     a summariser call has STARTED -- no state change yet
	//   compact        it finished; re-read summary and compacted_through
	//   compact_failed it did not finish; nothing changed
	//
	// The compacting/compact pair exists because compaction is not invisible to the user.
	// It runs after the reply so it never delays the turn that triggered it, but it OCCUPIES A
	// SLOT on the backend -- and on a single-slot llama-server (--parallel 1) the user's next
	// turn queues behind the summariser with no explanation. Unannounced, that is
	// indistinguishable from the model hanging. compact_failed exists so a client that showed
	// "compacting" has something to clear it with; without it a failed summary leaves the
	// indicator stuck on forever.
	//
	// Leaving room for kind:"delta" later, to relay partial tokens to other devices: a delta
	// has no seq (seq is assigned at commit) so it cannot share the commit path, and a client
	// that does not understand one can ignore it and still be correct.
	Kind string    `json:"kind"`
	Msgs []Message `json:"msgs"`
}

type Store struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-session write lock, so two devices cannot interleave writes

	submu   sync.Mutex
	subs    map[string]map[int64]chan Event
	nextSub int64
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}
	return &Store{
		dir:   dir,
		locks: map[string]*sync.Mutex{},
		subs:  map[string]map[int64]chan Event{},
	}, nil
}

// lockFor returns the per-session mutex. Whole-store locking would serialise unrelated
// conversations; no locking at all loses a turn when two devices post at once.
func (s *Store) lockFor(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.locks[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.locks[id] = m
	return m
}

// safeID rejects anything that could escape the session directory. The id reaches this from a
// URL path, so treating it as a filename without checking is a path-traversal read of any file
// the process can open.
func safeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func newID() string {
	// Time-ordered so a directory listing is chronological, with a random tail so two devices
	// creating a session in the same millisecond cannot collide. crypto/rand rather than
	// math/rand: the id is a URL path segment, and a guessable one lets anyone who reaches the
	// port read a conversation by enumeration.
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cannot happen on Linux, but a silent fallback to a predictable id would be worse
		// than a loud failure, so make it obvious in the id itself.
		return fmt.Sprintf("%d-norand", time.Now().UnixMilli())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

func (s *Store) Create(title string) (*Session, error) {
	now := time.Now().UnixMilli()
	sess := &Session{ID: newID(), Title: title, Created: now, Updated: now, NextSeq: 1}
	if err := s.write(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) write(sess *Session) error {
	b, err := json.MarshalIndent(sess, "", " ")
	if err != nil {
		return err
	}
	// Temp file plus rename: a crash or a full disk mid-write leaves the previous good file
	// rather than a truncated one. A conversation is not worth much if it can be half-saved.
	tmp := s.path(sess.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(sess.ID))
}

func (s *Store) Get(id string) (*Session, error) {
	if !safeID(id) {
		return nil, errors.New("bad session id")
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) List() ([]SessionMeta, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []SessionMeta{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := s.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // a corrupt or half-written file should not break the picker
		}
		out = append(out, SessionMeta{sess.ID, sess.Title, sess.Created, sess.Updated, len(sess.Messages)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// Delete removes a conversation and TELLS the other devices.
//
// Without the broadcast the failure is silent and total: another device's EventSource stays
// open and quiet, its next turn hits a session that no longer exists, the hook logs "chat
// referenced unknown session" and returns the no-op writer -- so chat keeps working perfectly
// while nothing is saved, and the only evidence is one line in the server log.
//
// Subscribers are released after the notice so they receive it before their channels close,
// and the per-session lock and subscriber map entries are dropped: nothing will ever reference
// this id again, and keeping them leaks an entry per deleted conversation for the process
// lifetime.
func (s *Store) Delete(id string) error {
	if !safeID(id) {
		return errors.New("bad session id")
	}
	if err := os.Remove(s.path(id)); err != nil {
		return err
	}
	s.broadcast(Event{Session: id, Kind: "deleted"})

	s.submu.Lock()
	for _, ch := range s.subs[id] {
		close(ch)
	}
	delete(s.subs, id)
	s.submu.Unlock()

	s.mu.Lock()
	delete(s.locks, id)
	s.mu.Unlock()
	return nil
}

func (s *Store) Rename(id, title string) error {
	m := s.lockFor(id)
	m.Lock()
	defer m.Unlock()
	sess, err := s.Get(id)
	if err != nil {
		return err
	}
	sess.Title = title
	sess.Updated = time.Now().UnixMilli()
	return s.write(sess)
}

// AppendUserTurn stores a user turn, skipping it if it is a retry of one that never got a reply.
//
// The user turn is persisted ON RECEIPT, before generation, so that a refresh mid-reply keeps
// the question. The cost of that choice is this: if the request then fails, the client pops the
// turn from its local history (index.html, the catch around the fetch) while the server has
// already written it. The user retypes, resends, and it lands a second time -- invisible until
// the next refresh, when the duplicate appears in the scrollback.
//
// Deduplicating on "the tail is an identical user turn with no assistant turn after it" rather
// than on a client-generated id, because the client cannot actually supply one honestly. It has
// no retry button; the user simply types the message again, and the page has no way to know
// whether that is a retry or a deliberate repeat. The server can tell, and precisely: an
// unanswered identical user turn at the tail is a failed attempt, whereas the same text sent
// twice on purpose has a reply between the two.
//
// Note the append-not-replace rule cannot help here. That protects against a stale device
// DELETING a turn; this is the same device adding one twice.
func (s *Store) AppendUserTurn(id, origin, content string) ([]Message, error) {
	sess, err := s.Get(id)
	if err == nil && len(sess.Messages) > 0 {
		last := sess.Messages[len(sess.Messages)-1]
		if last.Role == "user" && last.Content == content {
			return nil, nil // unanswered identical turn at the tail: this is a retry
		}
	}
	return s.Append(id, origin, Message{Role: "user", Content: content})
}

// Append adds turns, assigns their sequence numbers, persists, and notifies watchers.
//
// It APPENDS rather than replacing the stored transcript with whatever the client sent. The
// client posts its whole history with each request, so a replace would let a device holding a
// stale view silently delete a turn another device had just added. Appending lets two devices
// interleave instead. (It does not stop a stale device sending stale CONTEXT to the model --
// that is inherent to picking up a conversation elsewhere -- but it does stop data loss.)
// SetRoute records which backend and model a session is using. Called on each turn because it
// can change mid-conversation -- the picker lets you switch.
func (s *Store) SetRoute(id, backend, model string) {
	if backend == "" && model == "" {
		return
	}
	m := s.lockFor(id)
	m.Lock()
	defer m.Unlock()
	sess, err := s.Get(id)
	if err != nil || (sess.Backend == backend && sess.Model == model) {
		return
	}
	sess.Backend, sess.Model = backend, model
	_ = s.write(sess)
}

func (s *Store) Append(id, origin string, msgs ...Message) ([]Message, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	m := s.lockFor(id)
	m.Lock()
	sess, err := s.Get(id)
	if err != nil {
		m.Unlock()
		return nil, err
	}
	now := time.Now().UnixMilli()
	added := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		msg.Seq = sess.NextSeq
		msg.TS = now
		sess.NextSeq++
		sess.Messages = append(sess.Messages, msg)
		added = append(added, msg)
	}
	sess.Updated = now
	if sess.Title == "" {
		sess.Title = deriveTitle(msgs)
	}
	err = s.write(sess)
	m.Unlock()
	if err != nil {
		return nil, err
	}
	s.broadcast(Event{Session: id, Origin: origin, Kind: "commit", Msgs: added})
	return added, nil
}

// deriveTitle makes the picker readable. A list of timestamps is not a picker.
func deriveTitle(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.TrimSpace(strings.TrimPrefix(m.Content, "[voice] "))
		t = strings.Join(strings.Fields(t), " ")
		if len(t) > 48 {
			t = t[:48] + "…"
		}
		if t != "" {
			return t
		}
	}
	return ""
}

// Since returns messages after seq. limit<=0 means all of them; a positive limit returns the
// LAST limit messages, which is what a device joining a long conversation wants -- it needs
// enough to show, not the entire transcript.
func (s *Store) Since(id string, seq int64, limit int) ([]Message, int64, error) {
	sess, err := s.Get(id)
	if err != nil {
		return nil, 0, err
	}
	out := []Message{}
	for _, m := range sess.Messages {
		if m.Seq > seq {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, sess.NextSeq - 1, nil
}

// Subscribe returns a channel of events for one session and a function to release it.
//
// The channel is buffered and a full buffer DROPS rather than blocks: a wedged or paused device
// must not stall the request that is trying to persist a turn. A dropped event is recoverable --
// the client reconnects and asks for everything since its last seq -- whereas a blocked write is
// not.
func (s *Store) Subscribe(id string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	s.submu.Lock()
	s.nextSub++
	sid := s.nextSub
	if s.subs[id] == nil {
		s.subs[id] = map[int64]chan Event{}
	}
	s.subs[id][sid] = ch
	s.submu.Unlock()
	return ch, func() {
		s.submu.Lock()
		_, still := s.subs[id][sid]
		if m, ok := s.subs[id]; ok {
			delete(m, sid)
			if len(m) == 0 {
				delete(s.subs, id)
			}
		}
		s.submu.Unlock()
		// Only close what is still registered. Delete() closes every subscriber of a removed
		// session, and closing an already-closed channel panics -- which would take down the
		// server when a watcher of a deleted conversation happens to disconnect.
		if still {
			close(ch)
		}
	}
}

// Notify sends a message-less event (compaction lifecycle) to a session's watchers.
func (s *Store) Notify(id, kind string) { s.broadcast(Event{Session: id, Kind: kind}) }

func (s *Store) broadcast(ev Event) {
	s.submu.Lock()
	defer s.submu.Unlock()
	for _, ch := range s.subs[ev.Session] {
		select {
		case ch <- ev:
		default: // see Subscribe: dropping is recoverable, blocking is not
		}
	}
}
