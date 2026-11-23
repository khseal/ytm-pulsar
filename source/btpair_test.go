package main

import (
	"testing"
	"time"
)

func TestActionHoldIgnoresShortTap(t *testing.T) {
	var s pressState
	now := time.Unix(100, 0)
	if s.level(false, now) {
		t.Fatal("released button opened pairing")
	}
	if s.level(true, now) || s.level(true, now.Add(2*time.Second)) {
		t.Fatal("short hold opened pairing")
	}
	if !s.level(true, now.Add(btpairHold)) {
		t.Fatal("three second hold did not open pairing")
	}
	if s.level(true, now.Add(btpairHold+time.Second)) {
		t.Fatal("one hold opened pairing twice")
	}
	s.level(false, now.Add(5*time.Second))
	if s.level(true, now.Add(6*time.Second)) {
		t.Fatal("a new press opened pairing immediately")
	}
	if !s.level(true, now.Add(6*time.Second+btpairHold)) {
		t.Fatal("second hold did not open pairing")
	}
}
