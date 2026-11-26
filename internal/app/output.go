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

type Printer struct {
	Writer io.Writer
	JSON   bool
}

// clean prevents remote/database strings from injecting terminal control codes.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
func (p Printer) event(kind string, value any) error {
	return json.NewEncoder(p.Writer).Encode(struct {
		Event string `json:"event"`
		Data  any    `json:"data"`
	}{kind, value})
}
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
func (p Printer) Details(n namespace.Namespace) error {
	if p.JSON {
		return p.event("namespace", n)
	}
	_, err := fmt.Fprintf(p.Writer, "\nNAMESPACE DETAILS:\n-----------------------------------\nNAME:      %s\nACCOUNT:   %s\nROLE:      %s\nENV:       %s\n-----------------------------------\n", clean(n.Name), clean(n.Account), clean(n.Role), clean(n.Environment))
	return err
}
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
