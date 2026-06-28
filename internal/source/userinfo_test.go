package source

import "testing"

func TestParseUserinfo(t *testing.T) {
	u, ok := ParseUserinfo("upload=100; download=200; total=1000; expire=1785055788")
	if !ok {
		t.Fatalf("expected ok")
	}
	if u.Upload != 100 || u.Download != 200 || u.Total != 1000 || u.Expire != 1785055788 {
		t.Fatalf("parsed wrong: %+v", u)
	}
}

func TestParseUserinfoPartial(t *testing.T) {
	u, ok := ParseUserinfo("download=200; total=1000")
	if !ok {
		t.Fatalf("expected ok for partial")
	}
	if u.Download != 200 || u.Total != 1000 || u.Upload != 0 {
		t.Fatalf("partial wrong: %+v", u)
	}
}

func TestParseUserinfoEmpty(t *testing.T) {
	if _, ok := ParseUserinfo(""); ok {
		t.Fatalf("empty header should not be ok")
	}
}

func TestAggregateUserinfo(t *testing.T) {
	infos := []Userinfo{
		{Upload: 10, Download: 20, Total: 100, Expire: 2000},
		{Upload: 1, Download: 2, Total: 50, Expire: 1500},
		{Upload: 0, Download: 0, Total: 30, Expire: 0}, // expire=0 不参与取最早
	}
	a := AggregateUserinfo(infos)
	if a.Upload != 11 || a.Download != 22 || a.Total != 180 {
		t.Fatalf("sum wrong: %+v", a)
	}
	if a.Expire != 1500 {
		t.Fatalf("expire should be earliest non-zero, got %d", a.Expire)
	}
}

func TestUserinfoHeader(t *testing.T) {
	u := Userinfo{Upload: 1, Download: 2, Total: 3, Expire: 4}
	if got := u.Header(); got != "upload=1; download=2; total=3; expire=4" {
		t.Fatalf("header: %q", got)
	}
}
