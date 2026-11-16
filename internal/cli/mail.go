package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/canon"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/wire"
)

// maxBody is the largest message body the server takes.
const maxBody = 64 << 10

// announce is the target word of an announcement, and the scope word of a
// delegate allowed to post one.
const announce = "announce"

func (r *run) post(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	var paths, notify list
	fs.Var(&paths, "paths", "")
	fs.Var(&notify, "notify-read", "")
	name := fs.String("name", "", "")
	desc := fs.String("desc", "", "")
	by := fs.String("by", "", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return usagef(r.verb, "name the target and the subject: grelot post <announce|path|'@name'> <subject> --name <name> --paths '<path>[,<path>...]' --desc <text>, the body on stdin")
	}
	if ferr := r.requireFields(*name, *desc, paths); ferr != nil {
		return ferr
	}
	body, err := readBody(r.Stdin)
	if err != nil {
		return err
	}
	byMS, err := durationMS(r.verb, "by", *by)
	if err != nil {
		return err
	}
	req := wire.PostRequest{Subject: rest[1], Body: body, ByMS: byMS, Identity: wire.IdentityFields{Name: *name, Description: *desc}}
	to, err := r.target(ctx, rest[0])
	if err != nil {
		return err
	}
	req.To = rawJSON(to)
	if len(notify) > 0 {
		req.NotifyRead = rawJSON(notifyRead(notify))
	}
	if perr := r.postPaths(ctx, &req, paths); perr != nil {
		return perr
	}
	res, err := r.paths()
	if err != nil {
		return err
	}
	finding, local := res.ScanLocal(ctx, body)
	var got wire.Posted
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("createPost", nil), Body: req}, &got); err != nil {
		return unknownOutcome(err)
	}
	if *desc != noChange {
		r.descSent()
	}
	thread := ""
	if t, ok := to.(*wire.PostTarget); ok {
		thread = t.Path
	}
	r.posted(&got, rest[0], thread, finding, local)
	return nil
}

// postPaths sets a post's new paths in grelot form, with the where the
// server needs when they change.
func (r *run) postPaths(ctx context.Context, req *wire.PostRequest, paths list) error {
	if len(paths) == 0 {
		return nil
	}
	canonical, locs, err := r.canonical(ctx, paths)
	if err != nil {
		return err
	}
	req.Identity.Paths = canonical
	if len(locs) > 0 {
		w := r.placeOf(ctx, locs)
		req.Where = &w
	}
	return nil
}

// posted prints a post's answer, and the local-path warning when a
// recipient outside the sender's environment cannot read the path. thread
// is the path of a path-thread post, empty for any other target.
func (r *run) posted(got *wire.Posted, to, thread string, finding canon.Finding, local bool) {
	n := got.Recipients
	fmt.Fprintf(r.Stdout, "grelot: message %d posted to %s (thread %d): delivered to %d: %d same environment, %d other%s\n",
		got.Message, safe(to), got.Thread, n.Total, n.SameEnvironment, n.Other, wokenSplit(&n))
	if thread != "" && n.Total == 0 {
		history, naming := "", ""
		if w, ok := protopath.Word(safe(thread)); ok {
			history, naming = " (grelot history --path "+w+")", " and that command"
		}
		fmt.Fprintf(r.Stdout, "grelot: no other agent works in or follows %s yet, so nobody got this post now: "+
			"it stays in the thread's history while the thread lives, for anyone who follows later%s; "+
			"if one agent must act on it, send that agent a direct message naming message %d%s (grelot post '@<name>')\n",
			safe(thread), history, got.Message, naming)
	}
	if local && n.Other > 0 {
		fix := "send its content or a pushed ref"
		if finding.Canonical != "" {
			fix = "write " + finding.Canonical
		}
		fmt.Fprintf(r.Stdout, "grelot: %s cannot read %s: %s\n", recipients(n.Other), safe(finding.Token), fix)
	}
	for _, line := range got.Notices {
		fmt.Fprintln(r.Stdout, "grelot: "+safe(line))
	}
}

// wokenSplit is the "; 1 woken (@a), 4 quiet" tail of an addressed post's
// line, empty when every recipient was woken.
func wokenSplit(n *wire.Recipients) string {
	if len(n.Woken) == 0 {
		return ""
	}
	names := make([]string, len(n.Woken))
	for i, name := range n.Woken {
		names[i] = "@" + safe(name)
	}
	return fmt.Sprintf("; %d woken (%s), %d quiet", len(n.Woken), strings.Join(names, ", "), n.Quiet)
}

func recipients(n int) string {
	if n == 1 {
		return "1 recipient"
	}
	return strconv.Itoa(n) + " recipients"
}

// requireFields refuses a post missing an identity field before it is
// sent. A delegate's name is its handle and its scope replaces paths, so
// it declares only its description.
func (r *run) requireFields(name, desc string, paths list) error {
	missing := []string{}
	if name == "" && r.handle == "" {
		missing = append(missing, "--name")
	}
	if len(paths) == 0 && r.handle == "" {
		missing = append(missing, "--paths")
	}
	if desc == "" {
		missing = append(missing, "--desc")
	}
	if len(missing) == 0 {
		return nil
	}
	return usagef(r.verb, "every post declares %s: pass a new value, or %q to keep it", strings.Join(missing, ", "), noChange)
}

func readBody(in io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("grelot cannot read the body on stdin: %w", err)
	}
	if len(data) > maxBody {
		return "", errors.New("grelot: the body is over 64 KiB: shorten it, or post a pushed ref instead of the content; grelot post --help")
	}
	return string(data), nil
}

// target is a post's to: the announcements, an agent, or the path thread
// of a path in grelot form.
func (r *run) target(ctx context.Context, to string) (any, error) {
	switch {
	case to == announce:
		return announce, nil
	case strings.HasPrefix(to, "@"):
		return &wire.PostTarget{Agent: strings.TrimPrefix(to, "@")}, nil
	}
	canonical, _, err := r.canonical(ctx, []string{to})
	if err != nil {
		return nil, err
	}
	return &wire.PostTarget{Path: canonical[0]}, nil
}

// rawJSON is v as JSON, for a request member whose shape varies; v is a
// string, a string list or a wire type, which always encode.
func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// everyone is the --notify-read word for every recipient.
const everyone = "all"

// notifyRead is everyone, or the names listed without their @.
func notifyRead(names list) any {
	if len(names) == 1 && names[0] == everyone {
		return everyone
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.TrimPrefix(n, "@")
	}
	return out
}

// unknownOutcome reads a post's or reservation's timeout: no attempt of
// the call was answered in its budget, and the server may have done it.
// Running the command again sends a new key, so it would do it twice.
func unknownOutcome(err error) error {
	if ue, ok := errors.AsType[*client.UnreachableError](err); ok && ue.Timeout {
		return errors.New("grelot: the server did not answer in time, so the outcome is unknown: check grelot history --from '@me' or grelot reservations before you try again")
	}
	return err
}

func (r *run) inbox(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	limit := fs.Int("limit", 0, "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef(r.verb, "unexpected %q: grelot inbox takes only --limit", rest[0])
	}
	q, err := printPage(r.verb, *limit, "")
	if err != nil {
		return err
	}
	var page wire.Inbox
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: wire.Path("readInbox", nil), Query: q}, &page); err != nil {
		return err
	}
	return r.showPage(ctx, &page)
}

// showPage prints an inbox page, then acks up to its last seq, which marks
// what it printed read.
func (r *run) showPage(ctx context.Context, page *wire.Inbox) error {
	if len(page.Items) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no new mail")
		return nil
	}
	var upto uint64
	for i := range page.Items {
		writeEntry(r.Stdout, &page.Items[i])
		upto = max(upto, page.Items[i].Seq)
	}
	acked, err := r.ack(ctx, upto)
	ae, refused := errors.AsType[*client.APIError](err)
	refused = refused && ae.Upgrade != ""
	if err == nil || refused {
		client.NoteAck(r.ConfigDir, r.Version, refused)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case refused:
		fmt.Fprintln(r.Stdout, "grelot: the server refused the ack, so this page shows again and no watcher wakes for it: "+ae.Upgrade)
	case err != nil:
		fmt.Fprintln(r.Stdout, "grelot: the ack failed; this page may show again")
	case acked.Left.Total > 0:
		fmt.Fprintf(r.Stdout, "grelot: %d more unread: run grelot inbox again\n", acked.Left.Total)
	}
	return nil
}

// ack sends POST /v1/inbox/ack as one keyed call, so the client's re-send
// of a failed attempt answers the first result.
func (r *run) ack(ctx context.Context, upto uint64) (wire.Acked, error) {
	var got wire.Acked
	err := r.peek(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("ackInbox", nil), Body: wire.AckRequest{Upto: upto}}, &got)
	return got, err
}

func (r *run) history(ctx context.Context, args []string) error {
	q, err := r.historyQuery(ctx, args)
	if err != nil {
		return err
	}
	var got wire.History
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: wire.Path("getHistory", nil), Query: q}, &got); err != nil {
		return err
	}
	if len(got.Items) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no mail matches")
		return nil
	}
	for i := range got.Items {
		writeEntry(r.Stdout, &got.Items[i])
	}
	if got.NextCursor != "" {
		fmt.Fprintf(r.Stdout, "grelot: %d of %d shown; for the older ones, run grelot history again with the same flags and --cursor=%s\n",
			len(got.Items), got.Total, safe(got.NextCursor))
	}
	return nil
}

// printLimit is how many entries grelot inbox and grelot history print at
// once unless --limit says otherwise.
const printLimit = 20

// historyQuery maps history's flags to GET /v1/history's parameters.
func (r *run) historyQuery(ctx context.Context, args []string) (url.Values, error) {
	fs := flags(r.verb)
	text := map[string]*string{
		"thread": fs.String("thread", "", ""), "with": fs.String("with", "", ""), "from": fs.String("from", "", ""),
		"since": fs.String("since", "", ""), "until": fs.String("until", "", ""), "grep": fs.String("grep", "", ""),
	}
	local := map[string]*string{pathParam: fs.String("path", "", "")}
	limit := fs.Int("limit", 0, "")
	cursor := fs.String(wire.QueryCursor, "", "")
	set := boolFlags(fs, "announce", "full")
	set["last_read"] = fs.Bool("last-read", false, "")
	rest, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, usagef(r.verb, "unexpected %q: grelot history takes only flags", rest[0])
	}
	q, err := printPage(r.verb, *limit, *cursor)
	if err != nil {
		return nil, err
	}
	for name, v := range text {
		if *v != "" {
			q.Set(name, *v)
		}
	}
	if err := r.instants(q, "since", "until"); err != nil {
		return nil, err
	}
	setFlags(q, set)
	return q, r.canonicalQuery(ctx, q, local)
}

// instants turns each of q's names that is set into its RFC 3339 instant.
func (r *run) instants(q url.Values, names ...string) error {
	for _, name := range names {
		if v := q.Get(name); v != "" {
			at, err := instant(r.verb, name, v, r.now())
			if err != nil {
				return err
			}
			q.Set(name, at)
		}
	}
	return nil
}

// instant is a --since or --until value as the RFC 3339 instant the API
// takes: an RFC 3339 time as written, or a duration back from now, such as
// 2h, or a whole number of days, such as 2d, which a Go duration has no
// unit for.
func instant(verb, name, v string, now time.Time) (string, error) {
	if _, err := time.Parse(time.RFC3339, v); err == nil {
		return v, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d >= 0 && d <= maxDays {
			return now.Add(-time.Duration(d) * 24 * time.Hour).UTC().Format(time.RFC3339), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d).UTC().Format(time.RFC3339), nil
	}
	return "", usagef(verb, "--%s must be a duration such as --%s=2h, a number of days such as --%s=2d, or an RFC 3339 time", name, name, name)
}

// maxDays bounds a day count well past any message's retention.
const maxDays = 3650

// writeEntry prints one inbox or history entry, with the data label on
// its header line and the body quoted with "|".
func writeEntry(w io.Writer, e *wire.Entry) {
	var head []string
	if e.Message != 0 {
		head = append(head, fmt.Sprintf("message %d", e.Message))
	}
	if e.Thread != 0 {
		head = append(head, fmt.Sprintf("thread %d", e.Thread))
	}
	kind := safe(e.Kind)
	if e.ThreadPath != "" {
		kind += " " + safe(e.ThreadPath)
	}
	head = append(head, kind)
	if e.From != "" {
		head = append(head, "from @"+safe(e.From)+origin(e.Host, e.Relation))
	}
	if e.Via != "" {
		head = append(head, "copy via @"+safe(e.Via))
	}
	at := e.PostedAt
	if at.IsZero() {
		at = e.Written
	}
	head = append(head, stamp(at))
	fmt.Fprintln(w, strings.Join(head, " · ")+labelFor(e.Relation))
	if e.Subject != "" {
		fmt.Fprintf(w, "  subject: %s\n", safe(e.Subject))
	}
	if e.Text != "" {
		fmt.Fprintf(w, "  %s\n", safe(e.Text))
	}
	if body := strings.TrimRight(strings.ReplaceAll(e.Body, "\r\n", "\n"), "\n"); body != "" {
		for line := range strings.SplitSeq(body, "\n") {
			if line = safe(line); strings.TrimSpace(line) == "" {
				fmt.Fprintln(w, "  |")
				continue
			}
			fmt.Fprintf(w, "  | %s\n", line)
		}
	}
	if e.Truncated {
		fmt.Fprintf(w, "  (first line only: add --full for the whole body)\n")
	}
}

// origin is " (<host> · <relation>)", or "" when the server named neither.
func origin(host, relation string) string {
	var parts []string
	if host != "" {
		parts = append(parts, safe(host))
	}
	if relation != "" {
		parts = append(parts, safe(relation))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " · ") + ")"
}

func (r *run) receipts(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || !isID(rest[0]) {
		return usagef(r.verb, "name one message id: grelot receipts <message id>, the id grelot post, inbox and history print")
	}
	var got wire.Receipts
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: wire.Path("getReceipts", map[string]string{"id": rest[0]})}, &got); err != nil {
		return err
	}
	fmt.Fprintf(r.Stdout, "message %d · %s · posted %s · subject: %s%s\n", got.Message, safe(got.Kind), stamp(got.PostedAt), safe(got.Subject), dataLabel)
	count := map[string]int{}
	for i := range got.Recipients {
		v := &got.Recipients[i]
		count[v.State]++
		state := safe(v.State)
		if v.State == wire.ReceiptRead {
			state = "read at " + stamp(v.ReadAt)
		}
		fmt.Fprintf(r.Stdout, "  @%s%s · %s\n", safe(v.Name), origin(v.Host, v.Relation), state)
	}
	fmt.Fprintf(r.Stdout, "grelot: %d of %d have read; %d unread, %d dormant, %d retired\n", count[wire.ReceiptRead],
		len(got.Recipients), count[wire.ReceiptUnread], count[wire.ReceiptDormant], count[wire.ReceiptRetired])
	if got.RegisteredSince > 0 {
		fmt.Fprintf(r.Stdout, "grelot: %d identities registered since the post and never received it\n", got.RegisteredSince)
	}
	return nil
}

func (r *run) threads(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef(r.verb, "name at most one path: grelot threads [<path>]")
	}
	q := url.Values{}
	if len(rest) == 1 {
		canonical, _, cerr := r.canonical(ctx, rest)
		if cerr != nil {
			return cerr
		}
		q.Set(pathParam, canonical[0])
	}
	threads, err := walkList[wire.Thread](ctx, r.call, "listThreads", q)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no live path thread")
		return nil
	}
	for _, t := range threads {
		fmt.Fprintf(r.Stdout, "thread %d · %s · until %s%s\n", t.ID, safe(t.Path), stamp(t.Expires), dataLabel)
	}
	return nil
}

func (r *run) follow(ctx context.Context, args []string) error {
	return r.following(ctx, args, "follow", "following")
}

func (r *run) unfollow(ctx context.Context, args []string) error {
	return r.following(ctx, args, "unfollow", "no longer following")
}

// following runs the follow or unfollow row named row.
func (r *run) following(ctx context.Context, args []string, row, done string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name one path: grelot %s <path>", r.verb)
	}
	canonical, _, err := r.canonical(ctx, rest)
	if err != nil {
		return err
	}
	var got wire.IdentityAnswer
	method := http.MethodPut
	if row == "unfollow" {
		method = http.MethodDelete
	}
	req := &client.Request{Method: method, Path: wire.Path(row, nil), Query: url.Values{pathParam: {canonical[0]}}}
	if err := r.call(ctx, req, &got); err != nil {
		return err
	}
	follows := "nothing"
	if len(got.Identity.Follows) > 0 {
		follows = joined(got.Identity.Follows)
	}
	fmt.Fprintf(r.Stdout, "grelot: %s %s; @%s follows %s\n", done, canonical[0], safe(got.Identity.Name), follows)
	for _, line := range got.Notices {
		fmt.Fprintln(r.Stdout, "grelot: "+safe(line))
	}
	return nil
}
