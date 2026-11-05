package server

import (
	"testing"
	"time"
)

func TestPresence_onlineWhileAWaitIsOpenAndFor90sAfter(t *testing.T) {
	p := newPresence()
	const id = 7
	if p.online(id, testNow) {
		t.Error("online before any wait")
	}
	p.opened(id)
	if !p.online(id, testNow.Add(10*time.Minute)) {
		t.Error("offline while a wait is open")
	}
	p.opened(id)
	p.closed(id, testNow)
	if !p.online(id, testNow.Add(time.Hour)) {
		t.Error("offline while a second wait is still open")
	}
	p.closed(id, testNow)
	if !p.online(id, testNow.Add(89*time.Second)) {
		t.Error("offline 89 s after the last wait closed")
	}
	if p.online(id, testNow.Add(90*time.Second)) {
		t.Error("online 90 s after the last wait closed")
	}
	if p.online(id+1, testNow) {
		t.Error("another identity online")
	}
}
