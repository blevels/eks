package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"eks/internal/firecall"
	"eks/internal/namespace"
)

// Printer writes program results, not diagnostics, to Writer (normally stdout).
// JSON selects newline-delimited event/data records instead of human-readable
// text. Text fields have control characters replaced; JSON preserves field values
// using JSON escaping. The caller supplies and retains ownership of Writer.
type Printer struct {
	Writer io.Writer
	JSON   bool
}

// clean returns s with each Unicode control character replaced by a space,
// preventing remote/database strings from injecting terminal control codes into
// human-readable output. It leaves all other characters unchanged.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// event writes one newline-terminated JSON record to Writer, with kind as its
// event name and value as its data. It returns nil on success or the JSON
// encoding/writer error; it does not close Writer.
func (p Printer) event(kind string, value any) error {
	return json.NewEncoder(p.Writer).Encode(struct {
		Event string `json:"event"`
		Data  any    `json:"data"`
	}{kind, value})
}

// List writes items in order as a namespace table or a "namespaces" JSON record.
// An empty text list still includes the header; JSON preserves nil versus empty
// slices. It returns nil on success or an encoding, write, or table-flush error.
func (p Printer) List(items []namespace.Namespace) error {
	if p.JSON {
		return p.event("namespaces", items)
	}
	w := tabwriter.NewWriter(p.Writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tENV\tID\tACCT\tROLE"); err != nil {
		return err
	}
	for _, n := range items {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", clean(n.Name), clean(n.Environment), clean(n.ID), clean(n.Account), clean(n.Role)); err != nil {
			return err
		}
	}
	return w.Flush()
}

// Details writes n's namespace/account/role/environment as labeled text, or the
// full namespace value as a "namespace" JSON record. It returns nil on success
// or the encoding/writer error.
func (p Printer) Details(n namespace.Namespace) error {
	if p.JSON {
		return p.event("namespace", n)
	}
	_, err := fmt.Fprintf(p.Writer, "\nNAMESPACE DETAILS:\n-----------------------------------\nNAME:      %s\nACCOUNT:   %s\nROLE:      %s\nENV:       %s\n-----------------------------------\n", clean(n.Name), clean(n.Account), clean(n.Role), clean(n.Environment))
	return err
}

// Active writes each item's requester, role, and end time as text, or all items
// in one "active_firecalls" JSON record. Empty input writes no text. It returns
// nil on success or the first encoding/writer error.
func (p Printer) Active(items []firecall.Active) error {
	if p.JSON {
		return p.event("active_firecalls", items)
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(p.Writer, "ACTIVE FIRECALL DETAILS:\n-----------------------------------\nUSER:   %s\nROLE:   %s\nEND:    %s\n-----------------------------------\n", clean(item.Requester), clean(item.Role), clean(item.EndTime)); err != nil {
			return err
		}
	}
	return nil
}

// Created writes each item's firecall ID, status, and message as text, or all
// items in one "created_firecalls" JSON record. Empty input writes no text.
// It returns nil on success or the first encoding/writer error.
func (p Printer) Created(items []firecall.Created) error {
	if p.JSON {
		return p.event("created_firecalls", items)
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(p.Writer, "FIRECALL DETAILS:\nID:      %s\nSTATUS:  %s\nMESSAGE: %s\n", clean(item.ID), clean(item.Status), clean(item.Message)); err != nil {
			return err
		}
	}
	return nil
}

// Connected writes a connection confirmation for n.Name at t, either as text or
// a "connected" JSON record containing namespace and time. Time uses RFC3339
// with t's existing offset. It returns nil on success or the encoding/writer error.
func (p Printer) Connected(n namespace.Namespace, t time.Time) error {
	if p.JSON {
		return p.event("connected", struct {
			Namespace string `json:"namespace"`
			Time      string `json:"time"`
		}{n.Name, t.Format(time.RFC3339)})
	}
	_, err := fmt.Fprintf(p.Writer, "Connected to the EKS namespace %s\nLast Update: %s\n", clean(n.Name), t.Format(time.RFC3339))
	return err
}
