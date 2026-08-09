package eventbus

import (
	"strings"
	"time"
)

// Topic identifies a kind of event using dot-separated segments, ordered from
// most general to most specific:
//
//	docker.container.started
//	system.disk.threshold_exceeded
//	collector.run.failed
//
// The convention is <plugin>.<resource>.<action>. Ordering general-to-specific
// is what makes prefix subscription useful: a subscriber interested in
// everything Docker emits can match docker.** without enumerating resources
// that do not exist yet.
type Topic string

// Segments splits a topic into its dot-separated parts.
func (t Topic) Segments() []string { return strings.Split(string(t), ".") }

// Valid reports whether t is a well-formed topic: at least one segment, no
// empty segments, and no wildcard characters. Wildcards belong in a [Pattern],
// never in the topic of a published event.
func (t Topic) Valid() bool {
	if t == "" {
		return false
	}
	for _, seg := range t.Segments() {
		if seg == "" || strings.ContainsAny(seg, "*") {
			return false
		}
	}
	return true
}

// Event is a single occurrence observed somewhere in the platform.
//
// Events are immutable once published. Payload in particular must not be
// mutated by a subscriber: every subscriber receives the same value, so a
// mutation is visible to all of them and racy besides.
type Event struct {
	// ID uniquely identifies this event. Assigned by the bus when empty.
	ID string `json:"id"`
	// Topic classifies the event. Required.
	Topic Topic `json:"topic"`
	// Source names the component that observed the event, such as
	// "plugin.docker" or "collector.cpu".
	Source string `json:"source"`
	// Subject identifies the resource the event is about — a container id, a
	// mount point, a unit name. Optional, but it is what lets the incident
	// timeline group events by resource, so prefer to set it.
	Subject string `json:"subject,omitempty"`
	// Time is when the event was observed, not when it was published.
	// Assigned by the bus when zero.
	Time time.Time `json:"time"`
	// Payload carries topic-specific data. Subscribers type-assert on it
	// according to the topic contract documented alongside each publisher.
	Payload any `json:"payload,omitempty"`
}

// Pattern matches topics for subscription. Two wildcards are supported: a
// single star matches exactly one segment, and a double star matches zero or
// more remaining segments and must be the final segment.
//
// Examples:
//
//	docker.**                 every Docker event
//	docker.container.*        any container action, but not deeper topics
//	*.*.failed                any failure, from any plugin and resource
//	**                        every event on the bus
type Pattern string

// MatchAll subscribes to every topic.
const MatchAll Pattern = "**"

// Valid reports whether p is a well-formed pattern.
func (p Pattern) Valid() bool {
	if p == "" {
		return false
	}
	segs := strings.Split(string(p), ".")
	for i, seg := range segs {
		if seg == "" {
			return false
		}
		// "**" is only meaningful as a suffix: anything after it would be
		// unreachable, so accepting it would silently mislead the author.
		if seg == "**" && i != len(segs)-1 {
			return false
		}
		if strings.Contains(seg, "*") && seg != "*" && seg != "**" {
			return false
		}
	}
	return true
}

// Matches reports whether topic satisfies the pattern.
func (p Pattern) Matches(topic Topic) bool {
	pat := strings.Split(string(p), ".")
	seg := topic.Segments()

	for i, part := range pat {
		if part == "**" {
			return true // matches all remaining segments, including none
		}
		if i >= len(seg) {
			return false
		}
		if part != "*" && part != seg[i] {
			return false
		}
	}
	// Every pattern segment matched; the topic must not have extra segments,
	// otherwise docker.container would match docker.container.started.
	return len(pat) == len(seg)
}
