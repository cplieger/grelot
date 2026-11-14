//go:build !windows

package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cplieger/bolttable"
)

// The Conversations filters: every conversation, the announcement and
// path threads, or the direct threads.
const (
	ShowAll     = "all"
	ShowThreads = "threads"
	ShowDirect  = "direct"
)

// Web read page sizes: a limit of 0 takes the default.
const (
	defaultConversationsLimit = 50
	defaultPostsLimit         = 100
	maxWebLimit               = 100
	previewRunes              = 200
)

// ConversationsQuery filters Conversations. Show is ShowAll ("" too),
// ShowThreads or ShowDirect; Before and BeforeThread are the Next and
// NextThread of the page before, both 0 for the newest page.
type ConversationsQuery struct {
	Show         string
	Before       uint64
	BeforeThread uint64
	Limit        int
}

// ConversationThread names a thread for the web UI. Title is a path
// thread's path, "announce", or a direct thread's "@a and @b"; Agents are
// a direct thread's two names, sorted, each the identity's current name or,
// once its row is gone, the name its last post or delivery carried.
type ConversationThread struct {
	Kind   string
	Title  string
	Path   string
	Agents []string
	ID     uint64
}

// LastPost is a conversation's newest post: Preview is the body's first
// line, cut to previewRunes.
type LastPost struct {
	At      time.Time
	From    string
	Subject string
	Preview string
	ID      uint64
}

// ConversationSummary is one row of Conversations, over the posts its
// Conversation shows. Last is the newest of them; Participants counts the
// identities that sent or received them, a delegate's parent copies left
// out; Unread counts @user's unread ones.
type ConversationSummary struct {
	Last LastPost
	ConversationThread
	Participants int
	Unread       int
}

// ConversationPage is one page of Conversations, newest activity first.
// Next and NextThread are the Before and BeforeThread of the following
// page, both 0 on the last. Counts covers every page and every filter.
type ConversationPage struct {
	Rows       []ConversationSummary
	Counts     ConversationCounts
	Next       uint64
	NextThread uint64
}

// ConversationCounts counts the conversations each filter lists: Threads
// under ShowThreads, Direct under ShowDirect; ShowAll lists their sum.
type ConversationCounts struct {
	Threads int
	Direct  int
}

// ConversationPost is one post as the web UI reads it. From is the
// sender's name at the post, FromUser marks @user's; Origin is the path
// of the subpath thread a post in a parent path thread's conversation came
// from. Read counts the Recipients that have read it; Unread marks a post
// delivered to @user that @user has not read.
type ConversationPost struct {
	At         time.Time
	From       string
	Subject    string
	Body       string
	Origin     string
	Thread     uint64
	ID         uint64
	Read       int
	Recipients int
	FromUser   bool
	Unread     bool
}

// ReservationEntry is a grant or queue entry; Place is a queue entry's
// place among the entries overlapping it, 1 the oldest, and 0 on a grant.
type ReservationEntry struct {
	Reservation
	Place int
}

// Conversation is one thread for the web UI: its posts oldest first and
// its context. A path thread's conversation holds the posts of every live
// path thread at or under its path, the grants and queue entries at, above
// or under it (grants first, each part in id order), and its followers, the
// names of the identities whose work path or follow is at or above it. A
// direct thread's holds the card of each side whose identity remains.
// Next is the Before of the older page, 0 on the oldest.
type Conversation struct {
	Posts        []ConversationPost
	Reservations []ReservationEntry
	Followers    []string
	Cards        []Identity
	Thread       ConversationThread
	Next         uint64
}

// webReader is one web read's transaction, clock and @user's row, with the
// threads it has named so far.
type webReader struct {
	s       *Store
	r       *bolttable.ReadTx
	now     time.Time
	threads map[uint64]ConversationThread
	user    identityRow
}

// Conversations lists the announcement, path and direct threads whose
// conversation has an unexpired post, as @user reads them, newest last
// post first. Refusals: KindInvalid for an unknown Show, a limit outside 0
// to maxWebLimit, or a cursor missing one of its two parts.
func (s *Store) Conversations(ctx context.Context, q ConversationsQuery) (ConversationPage, error) {
	limit, lerr := webLimit(q.Limit, defaultConversationsLimit)
	if lerr != nil {
		return ConversationPage{}, lerr
	}
	kinds, kerr := showKinds(q.Show)
	if kerr != nil {
		return ConversationPage{}, kerr
	}
	if (q.Before == 0) != (q.BeforeThread == 0) {
		return ConversationPage{}, refuse(KindInvalid, "the page cursor needs both its post and its thread")
	}
	var out ConversationPage
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, err := s.webRead(r)
		if err == nil {
			out, err = wr.conversations(kinds, &q, limit)
		}
		return err
	})
	return out, err
}

func (wr *webReader) conversations(kinds []string, q *ConversationsQuery, limit int) (ConversationPage, error) {
	var out ConversationPage
	cs, counts, err := wr.candidates(kinds, q)
	out.Counts = counts
	if err != nil {
		return out, err
	}
	if len(cs) > limit {
		cs = cs[:limit]
		out.Next, out.NextThread = cs[limit-1].last.ID, cs[limit-1].th.ID
	}
	unread, err := wr.unreadByThread()
	if err != nil {
		return out, err
	}
	for i := range cs {
		row, err := wr.summary(&cs[i])
		if err != nil {
			return out, err
		}
		row.Unread = unread.in(&cs[i].th)
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// candidate is a listed thread with the threads its conversation shows and
// their newest post.
type candidate struct {
	threads []uint64
	th      threadRow
	last    messageRow
}

// newerFirst orders candidates by newest post, then by thread id, since a
// parent path conversation and its subpath's share their newest post.
func newerFirst(a, b *candidate) int {
	return cmp.Or(cmp.Compare(b.last.ID, a.last.ID), cmp.Compare(b.th.ID, a.th.ID))
}

// candidates lists the live threads of kinds whose conversation has an
// unexpired post, newerFirst, those after q's cursor only when it is set,
// and counts every listed kind's such threads, whatever kinds and q say.
func (wr *webReader) candidates(kinds []string, q *ConversationsQuery) ([]candidate, ConversationCounts, error) {
	var cs []candidate
	var counts ConversationCounts
	cursor := candidate{th: threadRow{ID: q.BeforeThread}, last: messageRow{ID: q.Before}}
	err := wr.s.t.threads.table.Scan(wr.r, bolttable.Range{}, func(_ uint64, th *threadRow) (bool, error) {
		if !slices.Contains(listedKinds, th.Kind) || th.Lease.due(wr.now) {
			return true, nil
		}
		c, found, err := wr.candidate(th)
		if !found {
			return err == nil, err
		}
		if th.Kind == kindDirect {
			counts.Direct++
		} else {
			counts.Threads++
		}
		if slices.Contains(kinds, th.Kind) && (q.Before == 0 || newerFirst(&cursor, &c) < 0) {
			cs = append(cs, c)
		}
		return err == nil, err
	})
	slices.SortFunc(cs, func(a, b candidate) int { return newerFirst(&a, &b) })
	return cs, counts, err
}

// candidate is th's row, found when its conversation has an unexpired post.
func (wr *webReader) candidate(th *threadRow) (candidate, bool, error) {
	threads, err := wr.s.conversationThreads(wr.r, th.ID, wr.now)
	if err != nil {
		return candidate{}, false, err
	}
	c := candidate{th: *th, threads: threads}
	found := false
	for _, id := range threads {
		last, ok, lerr := wr.lastPost(id)
		if lerr != nil {
			return c, false, lerr
		}
		if ok && (!found || last.ID > c.last.ID) {
			c.last, found = last, true
		}
	}
	return c, found, nil
}

// Conversation reads thread id's posts older than before (0 for the
// newest), up to limit, and its context. Refusals: KindInvalid for a limit
// outside 0 to maxWebLimit; KindNotFound for a thread that does not exist,
// has expired, or is the cards thread.
func (s *Store) Conversation(ctx context.Context, id, before uint64, limit int) (Conversation, error) {
	limit, lerr := webLimit(limit, defaultPostsLimit)
	if lerr != nil {
		return Conversation{}, lerr
	}
	var out Conversation
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, err := s.webRead(r)
		if err == nil {
			out, err = wr.conversation(id, before, limit)
		}
		return err
	})
	return out, err
}

func (wr *webReader) conversation(id, before uint64, limit int) (Conversation, error) {
	var out Conversation
	th, found, err := wr.s.t.threads.table.Get(wr.r, id)
	if err != nil {
		return out, err
	}
	if !found || th.Kind == kindCards || th.Lease.due(wr.now) {
		return out, refuse(KindNotFound, fmt.Sprintf("no conversation %d: it never existed or has expired", id))
	}
	if out.Thread, err = wr.conversationThread(&th); err != nil {
		return out, err
	}
	if out.Posts, out.Next, err = wr.threadPage(&th, before, limit); err != nil {
		return out, err
	}
	err = wr.context(&th, &out)
	return out, err
}

// MessageReceipts lists the recipients of message msg as @user sees
// them: every one, a delegate's included, but the parents' copies.
// Refusals: KindNotFound for a message that does not exist or has expired.
func (s *Store) MessageReceipts(ctx context.Context, msg uint64) (Receipts, error) {
	var out Receipts
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, err := s.webRead(r)
		if err == nil {
			out, err = wr.receipts(msg)
		}
		return err
	})
	return out, err
}

func (wr *webReader) receipts(msg uint64) (Receipts, error) {
	m, found, err := wr.s.messageByID(wr.r, msg, wr.now)
	if err != nil {
		return Receipts{}, err
	}
	if !found || !isPost(m.Kind) {
		return Receipts{}, refuse(KindNotFound, fmt.Sprintf("no message %d: it never existed or has expired", msg))
	}
	rows, err := wr.s.postDeliveries(wr.r, msg, wr.now)
	if err != nil {
		return Receipts{}, err
	}
	out := Receipts{PostedAt: m.PostedAt, Subject: m.Subject, Kind: m.Kind, Message: m.ID}
	for i := range rows {
		if rows[i].Via != "" {
			continue
		}
		rc, rerr := wr.s.receipt(wr.r, &wr.user, &m, &rows[i], wr.now)
		if rerr != nil {
			return out, rerr
		}
		out.Recipients = append(out.Recipients, rc)
	}
	if m.Kind == kindAnnounce {
		out.RegisteredSince, err = wr.s.registeredSince(wr.r, m.PostedAt, wr.now)
	}
	return out, err
}

func webLimit(limit, def int) (int, error) {
	switch {
	case limit == 0:
		return def, nil
	case limit < 0 || limit > maxWebLimit:
		return 0, refuse(KindInvalid, fmt.Sprintf("the page limit must be 1 to %d", maxWebLimit))
	}
	return limit, nil
}

// listedKinds are the thread kinds Conversations lists under ShowAll.
var listedKinds = []string{kindAnnounce, kindPath, kindDirect}

func showKinds(show string) ([]string, error) {
	switch show {
	case "", ShowAll:
		return listedKinds, nil
	case ShowThreads:
		return []string{kindAnnounce, kindPath}, nil
	case ShowDirect:
		return []string{kindDirect}, nil
	}
	return nil, refuse(KindInvalid, fmt.Sprintf("show must be %s, %s or %s", ShowAll, ShowThreads, ShowDirect))
}

func (s *Store) webRead(r *bolttable.ReadTx) (*webReader, error) {
	id, _, found, err := s.t.byName.Lookup(r, []byte(UserName))
	if err != nil {
		return nil, err
	}
	user, ufound, err := s.t.identities.table.Get(r, id)
	if err != nil {
		return nil, err
	}
	if !found || !ufound {
		return nil, errors.New("store: @user's identity row is missing")
	}
	return &webReader{s: s, r: r, now: s.now(), user: user, threads: map[uint64]ConversationThread{}}, nil
}

// unreadCounts are @user's unread posts by thread, with each path
// thread's path.
type unreadCounts struct {
	byThread map[uint64]int
	paths    map[uint64]string
}

// in counts th's conversation's unread posts: a path thread's takes in the
// threads under its path, as Conversation shows them.
func (u unreadCounts) in(th *threadRow) int {
	if th.Kind != kindPath {
		return u.byThread[th.ID]
	}
	n := 0
	for id, p := range u.paths {
		if under(p, th.Path) {
			n += u.byThread[id]
		}
	}
	return n
}

// unreadByThread counts @user's unread post deliveries by thread.
func (wr *webReader) unreadByThread() (unreadCounts, error) {
	out := unreadCounts{byThread: map[uint64]int{}, paths: map[uint64]string{}}
	err := wr.s.scanUnread(wr.r, wr.user.ID, wr.user.Read, wr.now, func(d *deliveryRow) error {
		if !isPost(d.Kind) || !d.ReadAt.IsZero() {
			return nil
		}
		out.byThread[d.Thread]++
		if d.Kind != kindPath {
			return nil
		}
		th, found, err := wr.s.t.threads.table.Get(wr.r, d.Thread)
		if found {
			out.paths[d.Thread] = th.Path
		}
		return err
	})
	return out, err
}

// lastPost is thread's newest unexpired post.
func (wr *webReader) lastPost(thread uint64) (messageRow, bool, error) {
	var out messageRow
	found := false
	err := wr.s.t.messages.table.Scan(wr.r, postRange(thread, 0), func(_ []byte, m *messageRow) (bool, error) {
		if m.Lease.due(wr.now) || !isPost(m.Kind) {
			return true, nil
		}
		out, found = *m, true
		return false, nil
	})
	return out, found, err
}

// postRange walks thread's messages newest first, those older than
// before only when before is not 0; message ids grow in post order.
func postRange(thread, before uint64) bolttable.Range {
	rg := bolttable.Range{Prefix: be64(thread), Reverse: true}
	if before != 0 {
		rg.To = pairKey(thread, before)
	}
	return rg
}

func (wr *webReader) summary(c *candidate) (ConversationSummary, error) {
	ct, err := wr.conversationThread(&c.th)
	if err != nil {
		return ConversationSummary{}, err
	}
	n, err := wr.participants(c.threads)
	last := &c.last
	return ConversationSummary{
		ConversationThread: ct, Participants: n,
		Last: LastPost{At: last.PostedAt, From: last.SenderName, Subject: last.Subject, Preview: preview(last.Body), ID: last.ID},
	}, err
}

// preview is body's first line, cut to previewRunes.
func preview(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSuffix(line, "\r")
	if utf8.RuneCountInString(line) <= previewRunes {
		return line
	}
	return string([]rune(line)[:previewRunes]) + "…"
}

// participants counts the senders and recipients of threads' unexpired
// posts, copies left out.
func (wr *webReader) participants(threads []uint64) (int, error) {
	ids := map[uint64]bool{}
	for _, thread := range threads {
		err := wr.s.t.messages.table.Scan(wr.r, bolttable.Range{Prefix: be64(thread)}, func(_ []byte, m *messageRow) (bool, error) {
			if m.Lease.due(wr.now) || !isPost(m.Kind) {
				return true, nil
			}
			ids[m.Sender] = true
			rows, err := wr.s.postDeliveries(wr.r, m.ID, wr.now)
			for i := range rows {
				if rows[i].Via == "" {
					ids[rows[i].Agent] = true
				}
			}
			return err == nil, err
		})
		if err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// conversationThread names th, once per read.
func (wr *webReader) conversationThread(th *threadRow) (ConversationThread, error) {
	if ct, ok := wr.threads[th.ID]; ok {
		return ct, nil
	}
	ct := ConversationThread{Kind: th.Kind, Path: th.Path, ID: th.ID}
	switch th.Kind {
	case kindAnnounce:
		ct.Title = announceKey
	case kindPath:
		ct.Title = th.Path
	case kindDirect:
		a, b, err := directSides(th)
		if err != nil {
			return ct, err
		}
		for _, id := range []uint64{a, b} {
			name, err := wr.nameIn(th.ID, id)
			if err != nil {
				return ct, err
			}
			ct.Agents = append(ct.Agents, name)
		}
		slices.Sort(ct.Agents)
		ct.Title = "@" + strings.Join(ct.Agents, " and @")
	}
	wr.threads[th.ID] = ct
	return ct, nil
}

// nameIn is identity id's current name or, once its row is gone, the name
// it carried in thread: as a post's sender, else as a delivery's recipient.
func (wr *webReader) nameIn(thread, id uint64) (string, error) {
	row, found, err := wr.s.t.identities.table.Get(wr.r, id)
	if err != nil {
		return "", err
	}
	if found && settled(&row, wr.now, wr.s.dormantTTL) != nil {
		return row.Name, nil
	}
	name := ""
	err = wr.s.t.messages.table.Scan(wr.r, bolttable.Range{Prefix: be64(thread), Reverse: true}, func(_ []byte, m *messageRow) (bool, error) {
		if m.Sender == id {
			name = m.SenderName
			return false, nil
		}
		rows, derr := wr.s.postDeliveries(wr.r, m.ID, wr.now)
		for i := range rows {
			if rows[i].Agent == id {
				name = rows[i].Name
			}
		}
		return derr == nil && name == "", derr
	})
	if name == "" && err == nil {
		err = fmt.Errorf("store: direct thread %d names identity %d, which no row or post names", thread, id)
	}
	return name, err
}

// threadPage reads th's posts older than before, up to limit, oldest
// first, with the posts of the live path threads under a path thread's
// path, and the Before of the older page.
func (wr *webReader) threadPage(th *threadRow, before uint64, limit int) ([]ConversationPost, uint64, error) {
	threads, err := wr.s.conversationThreads(wr.r, th.ID, wr.now)
	if err != nil {
		return nil, 0, err
	}
	var ms []messageRow
	for _, id := range threads {
		if ms, err = wr.newestPosts(ms, id, before, limit); err != nil {
			return nil, 0, err
		}
	}
	slices.SortFunc(ms, func(a, b messageRow) int { return cmp.Compare(b.ID, a.ID) })
	var next uint64
	if len(ms) > limit {
		ms = ms[:limit]
		next = ms[limit-1].ID
	}
	slices.Reverse(ms)
	out := make([]ConversationPost, 0, len(ms))
	for i := range ms {
		p, err := wr.post(&ms[i])
		if err != nil {
			return nil, 0, err
		}
		if th.Kind == kindPath && ms[i].Path != th.Path {
			p.Origin = ms[i].Path
		}
		out = append(out, p)
	}
	return out, next, nil
}

// newestPosts appends to ms up to limit+1 of thread's unexpired posts older
// than before, newest first, so a merge of several threads can tell
// whether an older page exists.
func (wr *webReader) newestPosts(ms []messageRow, thread, before uint64, limit int) ([]messageRow, error) {
	n := 0
	err := wr.s.t.messages.table.Scan(wr.r, postRange(thread, before), func(_ []byte, m *messageRow) (bool, error) {
		if !m.Lease.due(wr.now) && isPost(m.Kind) {
			ms = append(ms, *m)
			n++
		}
		return n <= limit, nil
	})
	return ms, err
}

// conversationThreads lists the threads thread id's conversation shows:
// for a live path thread, every live path thread at or under its path;
// otherwise id alone.
func (s *Store) conversationThreads(r *bolttable.ReadTx, id uint64, now time.Time) ([]uint64, error) {
	th, found, err := s.t.threads.table.Get(r, id)
	if err != nil || !found || th.Kind != kindPath || th.Lease.due(now) {
		return []uint64{id}, err
	}
	var out []uint64
	err = s.t.threadKey.Scan(r, bolttable.Range{Prefix: []byte(th.Path)}, func(ikey []byte, sub uint64, _ []byte) (bool, error) {
		if !under(string(ikey), th.Path) {
			return true, nil
		}
		v, live, gerr := s.t.threads.table.Get(r, sub)
		if live && v.Kind == kindPath && !v.Lease.due(now) {
			out = append(out, sub)
		}
		return gerr == nil, gerr
	})
	return out, err
}

// post is m with its read count.
func (wr *webReader) post(m *messageRow) (ConversationPost, error) {
	p := ConversationPost{
		At: m.PostedAt, From: m.SenderName, Subject: m.Subject, Body: m.Body,
		Thread: m.Thread, ID: m.ID, FromUser: m.Sender == wr.user.ID,
	}
	rows, err := wr.s.postDeliveries(wr.r, m.ID, wr.now)
	if err != nil {
		return p, err
	}
	for i := range rows {
		d := &rows[i]
		if d.Via != "" {
			continue
		}
		p.Recipients++
		read, err := wr.s.deliveryRead(wr.r, d, wr.now)
		if err != nil {
			return p, err
		}
		if read {
			p.Read++
		} else if d.Agent == wr.user.ID {
			p.Unread = true
		}
	}
	return p, nil
}

// context fills out's reservations and followers for a path thread, and
// its cards for a direct one.
func (wr *webReader) context(th *threadRow, out *Conversation) error {
	switch th.Kind {
	case kindPath:
		ids, err := overlapIDs(wr.r, wr.s.t.resvPath, th.Path)
		if err != nil {
			return err
		}
		if out.Reservations, err = wr.reservations(ids); err != nil {
			return err
		}
		out.Followers, err = wr.followers(th.Path)
		return err
	case kindDirect:
		var err error
		out.Cards, err = wr.directCards(th)
		return err
	}
	return nil
}

// directCards are the cards of direct thread th's sides whose identity
// remains, by name; a later identity under a gone side's name is not one.
func (wr *webReader) directCards(th *threadRow) ([]Identity, error) {
	a, b, err := directSides(th)
	if err != nil {
		return nil, err
	}
	var out []Identity
	for _, id := range []uint64{a, b} {
		row, found, err := wr.s.t.identities.table.Get(wr.r, id)
		if err != nil {
			return nil, err
		}
		if v := settled(&row, wr.now, wr.s.dormantTTL); found && v != nil {
			out = append(out, wr.s.view(v))
		}
	}
	slices.SortFunc(out, func(x, y Identity) int { return cmp.Compare(x.Name, y.Name) })
	return out, nil
}

// directSides are the two identity ids direct thread th's key names.
func directSides(th *threadRow) (a, b uint64, err error) {
	if _, err = fmt.Sscanf(th.Key, "direct:%d:%d", &a, &b); err != nil {
		err = fmt.Errorf("store: direct thread %d has key %q: %w", th.ID, th.Key, err)
	}
	return a, b, err
}

// reservations reads the unexpired rows of ids, grants first, each part in
// id order, every queue entry with its place.
func (wr *webReader) reservations(ids []uint64) ([]ReservationEntry, error) {
	queue, err := wr.queueEntries()
	if err != nil {
		return nil, err
	}
	var held, queued []ReservationEntry
	slices.Sort(ids)
	for _, id := range ids {
		v, found, err := wr.s.t.resv.table.Get(wr.r, id)
		if err != nil {
			return nil, err
		}
		if !found || v.Lease.due(wr.now) {
			continue
		}
		e := ReservationEntry{Reservation: wr.s.reservationView(wr.r, &identityRow{}, &v)}
		if v.State != reservationQueued {
			held = append(held, e)
			continue
		}
		e.Place = queuePlace(queue, &v)
		queued = append(queued, e)
	}
	return append(held, queued...), nil
}

// queueEntries lists every queue entry in queue order.
func (wr *webReader) queueEntries() ([]reservationRow, error) {
	var out []reservationRow
	err := wr.s.t.resvQueue.Scan(wr.r, bolttable.Range{}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		v, found, err := wr.s.t.resv.table.Get(wr.r, id)
		if found {
			out = append(out, v)
		}
		return err == nil, err
	})
	return out, err
}

// queuePlace is e's place among the queue entries overlapping it, as Who
// counts it: one plus the older entries that overlap it.
func queuePlace(queue []reservationRow, e *reservationRow) int {
	place := 1
	for i := range queue {
		if queue[i].ID < e.ID && overlaps(queue[i].Path, e.Path) {
			place++
		}
	}
	return place
}

// followers names, sorted, the live and dormant identities with a work
// path or follow at or above p, which its posts reach.
func (wr *webReader) followers(p string) ([]string, error) {
	ids := map[uint64]bool{}
	add := func(id uint64) (bool, error) {
		ids[id] = true
		return true, nil
	}
	for _, a := range ancestors(p) {
		if err := scanExact(wr.r, wr.s.t.byPath, a, add); err != nil {
			return nil, err
		}
		if err := scanExact(wr.r, wr.s.t.byFollow, a, add); err != nil {
			return nil, err
		}
	}
	var out []string
	for id := range ids {
		row, found, err := wr.s.recipientRow(wr.r, id, wr.now)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, row.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}
