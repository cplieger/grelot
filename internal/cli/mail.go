package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cplieger/grelot/internal/canon"
	"github.com/cplieger/grelot/internal/client"
)

// maxBody is the largest message body the server takes.
const maxBody = 64 << 10

// announce is the target word of an announcement, and the scope word of a
// delegate allowed to post one.
const announce = "announce"

// postBody is POST /v1/posts's body; To is announce or a *postTo.
type postBody struct {
	To         any           `json:"to"`
	NotifyRead any           `json:"notify_read,omitempty"`
	Where      *client.Where `json:"where,omitempty"`
	Subject    string        `json:"subject"`
	Body       string        `json:"body"`
	By         string        `json:"by,omitempty"`
	Identity   postIdentity  `json:"identity"`
}

// postTo is a post's thread other than the announcements.
type postTo struct {
	Path  string `json:"path,omitempty"`
	Agent string `json:"agent,omitempty"`
}

type postIdentity struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description"`
	Paths       []string `json:"paths,omitempty"`
}

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
		return usagef(r.verb, "name the target and the subject: grelot post <announce|path|@name> <subject> --name <n> --paths <p> --desc <d>, the body on stdin")
	}
	if ferr := r.requireFields(*name, *desc, paths); ferr != nil {
		return ferr
	}
	body, err := readBody(r.Stdin)
	if err != nil {
		return err
	}
	req := postBody{Subject: rest[1], Body: body, By: *by, Identity: postIdentity{Name: *name, Description: *desc}}
	if req.To, err = r.target(ctx, rest[0]); err != nil {
		return err
	}
	if len(notify) > 0 {
		req.NotifyRead = notifyRead(notify)
	}
	if perr := r.postPaths(ctx, &req, paths); perr != nil {
		return perr
	}
	res, err := r.paths()
	if err != nil {
		return err
	}
	finding, local := res.ScanLocal(ctx, body)
	var got client.Posted
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: "/v1/posts", Body: req}, &got); err != nil {
		return unknownOutcome(err)
	}
	if *desc != noChange {
		r.descSent()
	}
	thread := ""
	if to, ok := req.To.(*postTo); ok {
		thread = to.Path
	}
	r.posted(&got, rest[0], thread, finding, local)
	return nil
}

// postPaths sets a post's new paths in grelot form, with the where the
// server needs when they change.
func (r *run) postPaths(ctx context.Context, req *postBody, paths list) error {
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
func (r *run) posted(got *client.Posted, to, thread string, finding canon.Finding, local bool) {
	n := got.Recipients
	fmt.Fprintf(r.Stdout, "grelot: message %d posted to %s (thread %d): delivered to %d: %d same environment, %d other\n",
		got.Message, safe(to), got.Thread, n.Total, n.SameEnvironment, n.Other)
	if thread != "" && n.Total == 0 {
		fmt.Fprintf(r.Stdout, "grelot: no other agent works in or follows %s, so nobody got this post: "+
			"reach one agent with grelot post @<name> (grelot who lists them), or everyone with grelot post announce\n", safe(thread))
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
		return &postTo{Agent: strings.TrimPrefix(to, "@")}, nil
	}
	canonical, _, err := r.canonical(ctx, []string{to})
	if err != nil {
		return nil, err
	}
	return &postTo{Path: canonical[0]}, nil
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
		return errors.New("grelot: the server did not answer in time, so the outcome is unknown: check grelot history --from @me or grelot reservations before you try again")
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
	q := url.Values{}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	var page client.Inbox
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: "/v1/inbox", Query: q}, &page); err != nil {
		return err
	}
	return r.showPage(ctx, &page)
}

// showPage prints an inbox page, then acks up to its last seq, which marks
// what it printed read.
func (r *run) showPage(ctx context.Context, page *client.Inbox) error {
	if len(page.Entries) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no new mail")
		return nil
	}
	var upto uint64
	for i := range page.Entries {
		writeEntry(r.Stdout, &page.Entries[i])
		upto = max(upto, page.Entries[i].Seq)
	}
	acked, err := r.ack(ctx, upto)
	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil:
		fmt.Fprintln(r.Stdout, "grelot: the ack failed; this page may show again")
	case acked.Left.Total > 0:
		fmt.Fprintf(r.Stdout, "grelot: %d more unread: run grelot inbox again\n", acked.Left.Total)
	}
	return nil
}

// ack sends POST /v1/inbox/ack, retried, since read only moves forward.
func (r *run) ack(ctx context.Context, upto uint64) (client.Acked, error) {
	var got client.Acked
	err := r.peek(ctx, &client.Request{Method: http.MethodPost, Path: "/v1/inbox/ack", Body: map[string]uint64{"upto": upto}}, &got)
	return got, err
}

func (r *run) history(ctx context.Context, args []string) error {
	q, err := r.historyQuery(ctx, args)
	if err != nil {
		return err
	}
	var got client.History
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: "/v1/history", Query: q}, &got); err != nil {
		return err
	}
	if len(got.Entries) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no mail matches")
		return nil
	}
	for i := range got.Entries {
		writeEntry(r.Stdout, &got.Entries[i])
	}
	return nil
}

// historyQuery maps history's flags to GET /v1/history's parameters.
func (r *run) historyQuery(ctx context.Context, args []string) (url.Values, error) {
	fs := flags(r.verb)
	text := map[string]*string{
		"thread": fs.String("thread", "", ""), "with": fs.String("with", "", ""), "from": fs.String("from", "", ""),
		"since": fs.String("since", "", ""), "until": fs.String("until", "", ""), "grep": fs.String("grep", "", ""),
	}
	local := map[string]*string{pathParam: fs.String("path", "", "")}
	limit := fs.Int("limit", 0, "")
	set := map[string]*bool{"announce": fs.Bool("announce", false, ""), "full": fs.Bool("full", false, ""), "last_read": fs.Bool("last-read", false, "")}
	rest, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, usagef(r.verb, "unexpected %q: grelot history takes only flags", rest[0])
	}
	q := url.Values{}
	for name, v := range text {
		if *v != "" {
			q.Set(name, *v)
		}
	}
	for _, name := range []string{"since", "until"} {
		if v := q.Get(name); v != "" {
			q.Set(name, days(v))
		}
	}
	for name, v := range set {
		if *v {
			q.Set(name, "1")
		}
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	return q, r.canonicalQuery(ctx, q, local)
}

// days writes a whole number of days, such as 2d, as hours, since a Go
// duration has no day unit; any other value goes as written.
func days(v string) string {
	n, ok := strings.CutSuffix(v, "d")
	if !ok {
		return v
	}
	d, err := strconv.Atoi(n)
	if err != nil || d < 0 || d > maxDays {
		return v
	}
	return strconv.Itoa(d*24) + "h"
}

// maxDays bounds a day count well past any message's retention.
const maxDays = 3650

// writeEntry prints one inbox or history entry, with the data label on
// its header line and the body quoted with "|".
func writeEntry(w io.Writer, e *client.Entry) {
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
	var got client.Receipts
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: "/v1/messages/" + rest[0] + "/receipts"}, &got); err != nil {
		return err
	}
	fmt.Fprintf(r.Stdout, "message %d · %s · posted %s · subject: %s%s\n", got.Message, safe(got.Kind), stamp(got.PostedAt), safe(got.Subject), dataLabel)
	count := map[string]int{}
	for i := range got.Recipients {
		v := &got.Recipients[i]
		count[v.State]++
		state := safe(v.State)
		if v.State == client.ReceiptRead {
			state = "read at " + stamp(v.ReadAt)
		}
		fmt.Fprintf(r.Stdout, "  @%s%s · %s\n", safe(v.Name), origin(v.Host, v.Relation), state)
	}
	fmt.Fprintf(r.Stdout, "grelot: %d of %d have read; %d unread, %d dormant, %d retired\n", count[client.ReceiptRead],
		len(got.Recipients), count[client.ReceiptUnread], count[client.ReceiptDormant], count[client.ReceiptRetired])
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
	var got client.Threads
	if err := r.call(ctx, &client.Request{Method: http.MethodGet, Path: "/v1/threads", Query: q}, &got); err != nil {
		return err
	}
	if len(got.Threads) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no live path thread")
		return nil
	}
	for _, t := range got.Threads {
		fmt.Fprintf(r.Stdout, "thread %d · %s · until %s%s\n", t.ID, safe(t.Path), stamp(t.Expires), dataLabel)
	}
	return nil
}

func (r *run) follow(ctx context.Context, args []string) error {
	return r.following(ctx, args, http.MethodPut, "following")
}

func (r *run) unfollow(ctx context.Context, args []string) error {
	return r.following(ctx, args, http.MethodDelete, "no longer following")
}

func (r *run) following(ctx context.Context, args []string, method, done string) error {
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
	var got client.IdentityAnswer
	req := &client.Request{Method: method, Path: "/v1/follows", Query: url.Values{pathParam: {canonical[0]}}}
	if err := r.call(ctx, req, &got); err != nil {
		return err
	}
	follows := "nothing"
	if len(got.Identity.Follows) > 0 {
		follows = joined(got.Identity.Follows)
	}
	fmt.Fprintf(r.Stdout, "grelot: %s %s; @%s follows %s\n", done, canonical[0], safe(got.Identity.Name), follows)
	return nil
}
