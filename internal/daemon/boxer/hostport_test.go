package boxer

import "testing"

func TestHostPortsRoundTrip(t *testing.T) {
	ns := hostPortsToNullString([]int{1, 22, 3845})
	if !ns.Valid || ns.String != "1,22,3845" {
		t.Fatalf("hostPortsToNullString = %+v", ns)
	}
	got := hostPortsFromNullString(ns)
	if len(got) != 3 || got[0] != 1 || got[1] != 22 || got[2] != 3845 {
		t.Fatalf("round trip = %v", got)
	}
	if hostPortsToNullString(nil).Valid {
		t.Fatalf("empty should be NULL")
	}
	if hostPortsFromNullString(hostPortsToNullString(nil)) != nil {
		t.Fatalf("empty round trip should be nil")
	}
}
