package store

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// NotifyRead asks for a receipt watch on a post: on every recipient with
// All, or on the recipients named in Names, each of whom must be one. A
// nonzero By is a deadline, capped at the message's expiry, at which
// receipts_due names who has not read. The zero value asks for none.
type NotifyRead struct {
	Names []string
	By    time.Duration
	All   bool
}

func (n NotifyRead) set() bool { return n.All || len(n.Names) > 0 }

func (n NotifyRead) check() error {
	switch {
	case n.All && len(n.Names) > 0:
		return refuse(KindInvalid, "--notify-read takes all or a list of names, not both")
	case n.By < 0:
		return refuse(KindInvalid, "--by must be a positive duration")
	case n.By > 0 && !n.set():
		return refuse(KindInvalid, "--by needs --notify-read all or @names")
	}
	for _, name := range n.Names {
		if !namePattern.MatchString(name) && !isHandle(name) {
			return refuse(KindInvalid, fmt.Sprintf("--notify-read name %q is not an identity name or a delegate handle", name))
		}
	}
	return nil
}

// watching is the watch a post builds while it fans out.
type watching struct {
	// names maps each name asked for to whether it received the post; nil
	// asks for every recipient.
	names map[string]bool
	row   watchRow
}

// newWatch starts the watch n asks for on m, or returns nil.
func (w *wtx) newWatch(n NotifyRead, m *messageRow) (*watching, error) {
	if !n.set() {
		return nil, nil
	}
	id, err := w.s.t.watches.table.NextID(w.tx)
	if err != nil {
		return nil, err
	}
	wt := &watching{row: watchRow{ID: id, Message: m.ID, Sender: m.Sender, Subject: m.Subject, Lease: lease{Expires: m.Lease.Expires}}}
	if n.By > 0 {
		wt.row.By = true
		if by := w.now.Add(n.By); by.Before(m.Lease.Expires) {
			wt.row.Lease.Expires = by
		}
	}
	if !n.All {
		wt.names = map[string]bool{}
		for _, name := range n.Names {
			wt.names[name] = false
		}
	}
	return wt, nil
}

// add makes r a recipient wt waits for when wt asked for it, and returns
// the watch id r's delivery carries, 0 for none.
func (wt *watching) add(r *identityRow) uint64 {
	if wt == nil {
		return 0
	}
	if wt.names != nil {
		if _, asked := wt.names[r.Name]; !asked {
			return 0
		}
		wt.names[r.Name] = true
	}
	wt.row.Pending = append(wt.row.Pending, watchPending{ID: r.ID, Name: r.Name})
	return wt.row.ID
}

// putWatch stores wt once its post has fanned out. A name asked for that
// did not receive the post refuses the post, which rolls it back.
func (w *wtx) putWatch(wt *watching, out *Posted) error {
	if wt == nil {
		return nil
	}
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(wt.names)) {
		if !wt.names[name] {
			missing = append(missing, "@"+name)
		}
	}
	if len(missing) > 0 {
		return refuse(KindInvalid, fmt.Sprintf("--notify-read names who would not receive this post: %s; nothing was posted", strings.Join(missing, ", ")))
	}
	if len(wt.row.Pending) == 0 {
		out.Notices = append(out.Notices, "this post has no recipients, so no read notification was set")
		return nil
	}
	wt.row.Total = len(wt.row.Pending)
	return w.s.t.watches.table.Put(w.tx, wt.row.ID, &wt.row)
}

// watchExit is how a recipient leaves a watch.
type watchExit int

const (
	exitRead watchExit = iota
	exitRetired
)

// leaveWatch takes who out of watch id, counted by how, and completes the
// watch once nobody is left: it owes the sender receipts_complete and
// deletes the row, so a completed watch never fires again. A watch that
// is gone, due or no longer waiting for who changes nothing more.
func (w *wtx) leaveWatch(id uint64, who *identityRow, how watchExit) error {
	if id == 0 {
		return nil
	}
	v, found, err := w.s.t.watches.settle(w, id)
	if err != nil || !found {
		return err
	}
	i := slices.IndexFunc(v.Pending, func(p watchPending) bool { return p.ID == who.ID })
	if i < 0 {
		return nil
	}
	v.Pending = slices.Delete(v.Pending, i, i+1)
	if how == exitRetired {
		v.Retired++
	} else {
		v.Read++
	}
	if len(v.Pending) > 0 {
		return w.s.t.watches.table.Put(w.tx, id, &v)
	}
	if err := deleteRow(w, w.s.t.watches, id); err != nil {
		return err
	}
	text := fmt.Sprintf("message %d: all %d have read %s", v.Message, v.Total, v.Subject)
	if v.Retired > 0 {
		text = fmt.Sprintf("message %d: %d of %d have read %s; %d retired without reading", v.Message, v.Read, v.Total, v.Subject, v.Retired)
	}
	w.oweEvent(v.Sender, kindReceiptsComplete, text)
	return nil
}

// leaveWatches ends the watches row sent and takes row out, counted
// retired, of each watch its unread deliveries name. Call it before row
// is deleted: its deliveries can no longer be read.
func (w *wtx) leaveWatches(row *identityRow) error {
	var sent []uint64
	err := w.s.t.watchBy.Scan(w.r, bolttable.Range{Prefix: be64(row.ID)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		sent = append(sent, id)
		return true, nil
	})
	if err != nil {
		return err
	}
	for _, id := range sent {
		if err := deleteRow(w, w.s.t.watches, id); err != nil {
			return err
		}
	}
	var named []uint64
	if err := w.s.scanUnread(w.r, row.ID, row.Read, w.now, func(d *deliveryRow) error {
		if d.Watch != 0 {
			named = append(named, d.Watch)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range named {
		if err := w.leaveWatch(id, row, exitRetired); err != nil {
			return err
		}
	}
	return nil
}

// expireWatch ends watch v at its deadline or its message's expiry; a
// --by watch first owes its sender receipts_due.
func expireWatch(w *wtx, k uint64, v *watchRow) error {
	if err := deleteRow(w, w.s.t.watches, k); err != nil {
		return err
	}
	if !v.By {
		return nil
	}
	var unread, dormant []string
	retired := v.Retired
	for _, p := range v.Pending {
		r, found, err := w.s.recipientRow(w.r, p.ID, w.now)
		switch {
		case err != nil:
			return err
		case !found:
			retired++
		case r.State == stateDormant:
			dormant = append(dormant, "@"+p.Name)
		default:
			unread = append(unread, "@"+p.Name)
		}
	}
	text := fmt.Sprintf("message %d: %d of %d have read %s by the deadline", v.Message, v.Read, v.Total, v.Subject)
	if len(unread) > 0 {
		text += "; still unread: " + strings.Join(unread, ", ")
	}
	if len(dormant) > 0 {
		text += "; dormant: " + strings.Join(dormant, ", ")
	}
	if retired > 0 {
		text += fmt.Sprintf("; %d retired without reading", retired)
	}
	w.oweEvent(v.Sender, kindReceiptsDue, text)
	return nil
}
