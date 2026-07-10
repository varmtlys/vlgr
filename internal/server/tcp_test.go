package server

import "testing"

func TestPortAllocator_AutoAndRelease(t *testing.T) {
	a := NewPortAllocator(20000, 20002)
	p1, err := a.Allocate(0)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	p2, _ := a.Allocate(0)
	p3, _ := a.Allocate(0)
	if p1 == p2 || p2 == p3 || p1 == p3 {
		t.Errorf("allocated duplicate ports: %d %d %d", p1, p2, p3)
	}
	if _, err := a.Allocate(0); err == nil {
		t.Error("expected exhaustion error")
	}
	a.Release(p2)
	if p, err := a.Allocate(0); err != nil || p != p2 {
		t.Errorf("released port not reused: got %d err %v", p, err)
	}
}

func TestPortAllocator_Preferred(t *testing.T) {
	a := NewPortAllocator(20000, 20010)
	if _, err := a.Allocate(19999); err == nil {
		t.Error("expected out-of-range error")
	}
	p, err := a.Allocate(20005)
	if err != nil || p != 20005 {
		t.Fatalf("preferred allocate: got %d err %v", p, err)
	}
	if _, err := a.Allocate(20005); err == nil {
		t.Error("expected in-use error")
	}
}
