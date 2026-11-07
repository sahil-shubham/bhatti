package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/store"
	"github.com/spf13/cobra"
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Show or follow events recorded for your account",
	Long: `Show the events recorded against your own account.

Events are numbered, and --after N continues after event N, so a client that
remembers the last number it saw misses nothing. The server prunes old events
(its retention), so a very old number can fall off the front; the command says
so when that happens.

Without --follow it prints up to 1000 events, from the oldest still retained or
from --after, and tells you how to continue if there are more. With --follow it
keeps printing events as they happen: without --after it starts at the live
edge (only new events), while --follow --after 0 replays everything retained
first. A followed stream that the server ends exits non-zero and prints the
--after value to resume from.

Each line is: id, local time, type, sandbox, details. --json prints the records
exactly as the server sent them, one per line.`,
	Example: `  bhatti events                          # oldest retained events
  bhatti events --after 1200             # everything after event 1200
  bhatti events --follow                 # new events as they happen
  bhatti events --follow --after 0       # replay everything, then follow
  bhatti events --type thermal --sandbox dev
  bhatti events --follow --json | jq -r .type`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := eventsOptionsFromFlags(cmd)
		if err != nil {
			return err
		}
		return streamEvents(cmd.Context(), os.Stdout, os.Stderr, opts)
	},
}

func init() { addEventsFlags(eventsCmd) }

func addEventsFlags(c *cobra.Command) {
	c.Flags().Int64("after", 0, "Only events after this event number")
	c.Flags().BoolP("follow", "f", false, "Keep streaming new events")
	c.Flags().String("type", "", "Event type prefix (thermal matches thermal.*)")
	c.Flags().String("sandbox", "", "Only events of this sandbox (name or ID)")
	c.RegisterFlagCompletionFunc("sandbox", completeSandboxNames)
}

func eventsOptionsFromFlags(cmd *cobra.Command) (eventsOptions, error) {
	opts := eventsOptions{
		afterSet: cmd.Flags().Changed("after"),
		json:     isJSON(cmd),
	}
	opts.after, _ = cmd.Flags().GetInt64("after")
	opts.follow, _ = cmd.Flags().GetBool("follow")
	opts.typePrefix, _ = cmd.Flags().GetString("type")
	if s, _ := cmd.Flags().GetString("sandbox"); s != "" {
		id, err := resolveID(s)
		if err != nil {
			return opts, err
		}
		opts.sandbox = id
	}
	return opts, nil
}

// eventsOptions is a parsed `bhatti events` invocation. afterSet separates
// "--after 0" (replay everything, also when following) from no --after (start
// at the oldest retained event, or at the live edge when following).
type eventsOptions struct {
	after      int64
	afterSet   bool
	follow     bool
	typePrefix string
	sandbox    string // already resolved
	json       bool
}

func (o eventsOptions) query() url.Values {
	q := url.Values{}
	if o.afterSet {
		q.Set("after", strconv.FormatInt(o.after, 10))
	}
	if o.follow {
		q.Set("follow", "1")
	}
	if o.typePrefix != "" {
		q.Set("type", o.typePrefix)
	}
	if o.sandbox != "" {
		q.Set("sandbox", o.sandbox)
	}
	return q
}

// filterFlags renders the user's own --type/--sandbox back as flags, so a
// continuation or resume hint can be pasted and still mean the same stream.
func (o eventsOptions) filterFlags() string {
	var b strings.Builder
	if o.typePrefix != "" {
		b.WriteString(" --type " + shellWord(o.typePrefix))
	}
	if o.sandbox != "" {
		b.WriteString(" --sandbox " + shellWord(o.sandbox))
	}
	return b.String()
}

func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/:-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// eventRecord is one line of the stream: an event, or a control record that
// carries no id and a reserved "events." type (events.gap, events.more).
type eventRecord struct {
	store.Event
	After  int64 `json:"after"`
	Oldest int64 `json:"oldest"`
}

func (r eventRecord) isControl() bool { return strings.HasPrefix(r.Type, "events.") }

// streamEvents requests /events and writes the records to out as they arrive:
// each is handled as soon as it is complete, so a followed stream is live. In
// human mode control records and notices go to errOut so out stays one event
// per line.
func streamEvents(ctx context.Context, out, errOut io.Writer, o eventsOptions) error {
	if o.after < 0 {
		return fmt.Errorf("--after must not be negative")
	}
	target := apiURL + "/events"
	if q := o.query(); len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-ndjson")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	checkServerVersion(resp)
	if resp.StatusCode >= 400 {
		return apiErrorFromResponse(resp)
	}

	var (
		dec     = json.NewDecoder(resp.Body)
		raw     json.RawMessage
		line    []byte
		lastID  int64
		printed int
	)
	for {
		raw = raw[:0]
		if err := dec.Decode(&raw); err != nil {
			return streamEnded(o, lastID, printed, errOut, err)
		}
		var rec eventRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("malformed record in event stream: %w", err)
		}
		isEvent := !rec.isControl()
		if isEvent && rec.ID > lastID {
			lastID = rec.ID
		}

		if o.json {
			line = append(append(line[:0], raw...), '\n')
			if _, err := out.Write(line); err != nil {
				return err
			}
			continue
		}

		switch {
		case isEvent:
			if _, err := io.WriteString(out, eventLine(rec.Event)); err != nil {
				return err
			}
			printed++
		case rec.Type == "events.gap":
			fmt.Fprintf(errOut, "events after #%d up to the oldest stored #%d were pruned by retention\n", rec.After, rec.Oldest)
		case rec.Type == "events.more":
			fmt.Fprintf(errOut, "more events: continue with --after %d%s\n", rec.After, o.filterFlags())
		}
	}
}

// streamEnded turns the end of the body into the command's result. A listing
// that ends is done; a follow that ends is not, since it never finishes by
// itself — the server went away — so it fails and names where to resume.
func streamEnded(o eventsOptions, lastID int64, printed int, errOut io.Writer, err error) error {
	clean := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if !o.follow {
		// A cut-off listing must not pass for a complete one: the user would
		// continue from a cursor that skips what never arrived.
		if err != io.EOF {
			return fmt.Errorf("event stream interrupted: %w", err)
		}
		if printed == 0 && !o.json {
			fmt.Fprintln(errOut, "No events.")
		}
		return nil
	}
	resume := "bhatti events --follow"
	switch {
	case lastID > 0:
		resume += fmt.Sprintf(" --after %d", lastID)
	case o.afterSet:
		resume += fmt.Sprintf(" --after %d", o.after)
	}
	resume += o.filterFlags()
	if clean {
		return fmt.Errorf("event stream ended (server closed the connection); resume with: %s", resume)
	}
	return fmt.Errorf("event stream failed: %v; resume with: %s", err, resume)
}

// eventLine is "id  time  type  sandbox  details", id first because it is the
// value --after takes. Columns follow `bhatti admin events`.
func eventLine(e store.Event) string {
	ts := e.Timestamp.Local().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%-8d %-20s %-24s %-16s %s", e.ID, ts, e.Type, eventSandboxLabel(e), formatEventDetails(e))
	return strings.TrimRight(line, " ") + "\n"
}
