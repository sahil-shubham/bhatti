package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// eventsPageLimit bounds one read of the events table: the whole answer to a
// request without follow, and each read a follow stream makes while it catches
// up.
const eventsPageLimit = 1000

// The types of the two records in the event stream that are not events.
const (
	eventsGapType  = "events.gap"
	eventsMoreType = "events.more"
)

// sandboxIDRe is the shape genID gives a sandbox ID.
var sandboxIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// eventsControl is a record in the event stream that is not an event: it has no
// id and its type is reserved under "events.".
type eventsControl struct {
	Type   string `json:"type"`
	After  int64  `json:"after"`
	Oldest int64  `json:"oldest,omitempty"`
}

// EndEventStreams ends every open /events follow stream, and any opened later.
// The daemon calls it as HTTP shutdown begins: http.Server.Shutdown waits for
// each request to finish, so followers must also be interrupted when blocked
// writing to clients that stopped reading. Close ends them too.
func (s *Server) EndEventStreams() { s.endEventStreams() }

// handleEvents serves GET /events: the caller's own events from the events
// table as NDJSON, one event per line in id order. A line is the store.Event
// JSON, and its id is the cursor: a client that keeps the highest id it has
// seen and passes it as after sees nothing twice and misses nothing that was
// stored, across disconnects and daemon restarts (a gap record says when
// retention removed some).
//
//	GET /events?after=<id>&type=<prefix>&sandbox=<id|name>&follow=<bool>
//	HEAD /events answers the same content headers without replaying or following.
//
//	after    only events with a greater id. Omitted, a request without follow
//	         starts at the oldest retained event and a follow stream starts
//	         at the live edge, with only events stored from then on. after=0
//	         asks for everything retained, with or without follow. Not a
//	         non-negative integer: 400.
//	type     only events whose type starts with this string.
//	sandbox  only this sandbox's events: an ID, or the name of one of the
//	         caller's sandboxes. An ID that names no sandbox is used as given,
//	         since the events of a destroyed sandbox outlive it; anything else
//	         that names none is 404.
//	follow   keep the response open and write each event as it is stored, until
//	         the client disconnects or the server shuts down (the body then
//	         ends cleanly). Without it the response is the backlog, at most
//	         eventsPageLimit events, and ends. Not a boolean: 400.
//
// Only events whose user_id is the caller's are served. Events with no owner,
// such as failed logins and daemon start/stop, are never served to anyone.
//
// Two records in the stream are not events. They have no id, their type is
// reserved under "events.", and the type filter does not apply to them:
//
//	{"type":"events.gap","after":N,"oldest":M}
//	    Before each page whose scan cursor is N>0, if the oldest event still
//	    stored is M>N+1: retention deleted the events between, so the stream
//	    is not continuous there. Pruning ignores who owns a row, so this can
//	    overstate what the caller lost (those ids may have been other users')
//	    but never understate it. Following streams also advance past events
//	    rejected by their filters, so already-scanned history is not a gap.
//	{"type":"events.more","after":K}
//	    The last line of a response without follow that stopped at the page
//	    limit; K is the id of the last event sent, and the next page is
//	    after=K. A response without it is the end of what matched. A follow
//	    stream never has it, since it keeps reading until it is caught up.
//
// Event ids come from the table, so delivery is at-least-once from the
// caller's own cursor: a slow client only slows its own response, and an event
// stored while the client was away is delivered when it next reads.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)
	params := r.URL.Query()
	after, afterGiven, follow, err := parseEventsParams(params)
	if err != nil {
		errResp(w, 400, err.Error())
		return
	}
	query := store.EventQuery{UserID: user.ID, After: after, TypePrefix: params.Get("type"), Limit: eventsPageLimit}
	if params.Has("sandbox") {
		ref := params.Get("sandbox")
		id, err := s.eventsSandbox(ctx, user.ID, ref)
		if errors.Is(err, store.ErrNotFound) {
			errResp(w, 404, fmt.Sprintf("sandbox %q not found", ref))
			return
		}
		if err != nil {
			errRespInternal(w, r, "resolve events sandbox failed", err)
			return
		}
		query.SandboxID = id
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		errResp(w, 500, "streaming not supported")
		return
	}
	out := &eventsWriter{w: w, flusher: flusher, enc: json.NewEncoder(w)}
	if r.Method == http.MethodHead {
		out.begin()
		return
	}
	// Only a follow stream waits, and without a recorder nothing would wake it.
	rec := s.events
	if follow && rec == nil {
		errResp(w, http.StatusServiceUnavailable, "event recorder is not running")
		return
	}

	if !afterGiven && follow {
		newest, err := s.store.NewestEventID(ctx)
		if err != nil {
			errRespInternal(w, r, "read event bounds failed", err)
			return
		}
		query.After = newest
	}

	if follow {
		s.followEvents(ctx, r, out, rec, query)
	} else {
		s.pageEvents(ctx, r, out, query)
	}
}

// pageEvents answers a request without follow: the events after the cursor, up
// to the page limit, and the pointer to the next page when there are more.
func (s *Server) pageEvents(ctx context.Context, r *http.Request, out *eventsWriter, query store.EventQuery) {
	// One more than a page, to know whether another page exists.
	query.Limit = eventsPageLimit + 1
	page, err := s.store.EventsAfter(ctx, query)
	if err != nil {
		errRespInternal(out.w, r, "read events failed", err)
		return
	}
	events := page.Events
	more := len(events) > eventsPageLimit
	if more {
		events = events[:eventsPageLimit]
	}
	out.begin()
	out.gap(query.After, page.Oldest)
	out.events(events)
	if more {
		out.control(eventsControl{Type: eventsMoreType, After: events[len(events)-1].ID})
	}
	out.flush()
}

// followEvents streams events as they are stored until the client goes or the
// server shuts down. Each pass reads from the cursor until caught up, and the
// recorder's tail runs the next pass after the next flush.
func (s *Server) followEvents(ctx context.Context, r *http.Request, out *eventsWriter, rec *EventRecorder, query store.EventQuery) {
	if s.eventStreamCtx.Err() != nil {
		out.begin()
		out.flush()
		return
	}
	// One context for every way the stream ends: the client going, Close, and
	// EndEventStreams.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(s.eventStreamCtx, stop)()
	controller := http.NewResponseController(out.w)
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		// Cancellation must interrupt Encode/Flush as well as database reads
		// and idle waits. Slow readers have no deadline during normal use.
		controller.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stopInterrupt() {
			// Wait before clearing: an idle stream can then finish its HTTP
			// framing cleanly, and no callback can affect a reused connection.
			<-interrupted
			controller.SetWriteDeadline(time.Time{})
		}
	}()

	err := rec.tail(ctx, func() error {
		for {
			page, err := s.store.EventsAfter(ctx, query)
			if err != nil {
				return err
			}
			// The first pass always answers, even with nothing to send, so the
			// client has its response headers now rather than at the first event.
			if !out.started {
				out.begin()
			}
			out.gap(query.After, page.Oldest)
			out.events(page.Events)
			if !out.flush() {
				return out.err
			}
			query.After = page.ScannedThrough
			if len(page.Events) < eventsPageLimit {
				return nil
			}
		}
	})
	switch {
	case out.err != nil || ctx.Err() != nil:
		// The client left or the server is shutting down: how streams end.
	case !out.started:
		errRespInternal(out.w, r, "read events failed", err)
	default:
		slog.Error("events.stream_failed", "request_id", RequestIDFromContext(ctx),
			"user_id", query.UserID, "after", query.After, "error", err)
	}
}

// eventsWriter writes the records of one /events response. A write error means
// the client is gone; it is kept and reported by flush, which is where each
// batch ends.
type eventsWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	enc     *json.Encoder
	started bool
	err     error
}

// begin sends the response headers.
func (o *eventsWriter) begin() {
	o.w.Header().Set("Content-Type", "application/x-ndjson")
	o.w.Header().Set("X-Content-Type-Options", "nosniff")
	o.w.WriteHeader(http.StatusOK)
	o.started = true
}

func (o *eventsWriter) gap(after, oldest int64) {
	if after > 0 && after < oldest-1 {
		o.control(eventsControl{Type: eventsGapType, After: after, Oldest: oldest})
	}
}

func (o *eventsWriter) control(c eventsControl) {
	if o.err == nil {
		o.err = o.enc.Encode(c)
	}
}

func (o *eventsWriter) events(events []store.Event) {
	for i := range events {
		if o.err != nil {
			return
		}
		o.err = o.enc.Encode(&events[i])
	}
}

// flush pushes what was written to the client and reports whether the
// connection is still good.
func (o *eventsWriter) flush() bool {
	if o.err == nil {
		o.flusher.Flush()
	}
	return o.err == nil
}

// parseEventsParams reads after and follow. A parameter that is present must be
// valid: an empty after would otherwise silently replay everything.
func parseEventsParams(q url.Values) (after int64, afterGiven, follow bool, err error) {
	if q.Has("after") {
		after, err = strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || after < 0 {
			return 0, false, false, errors.New("after must be a non-negative event id")
		}
		afterGiven = true
	}
	if q.Has("follow") {
		if follow, err = strconv.ParseBool(q.Get("follow")); err != nil {
			return 0, false, false, errors.New("follow must be true or false")
		}
	}
	return after, afterGiven, follow, nil
}

// eventsSandbox maps the sandbox parameter to the sandbox ID events are
// filtered by. Events carry the ID, not the name: a name can be reused after the
// sandbox is destroyed, and the new one is not the old one's history.
func (s *Server) eventsSandbox(ctx context.Context, userID, ref string) (string, error) {
	sb, err := s.store.GetSandbox(ctx, userID, ref)
	switch {
	case err == nil:
		return sb.ID, nil
	case errors.Is(err, store.ErrNotFound) && sandboxIDRe.MatchString(ref):
		return ref, nil
	default:
		return "", err
	}
}
