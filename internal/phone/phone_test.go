package phone

import "testing"

func TestParseADBDevices(t *testing.T) {
	out := "List of devices attached\nR58M123 device usb:1-1 product:x model:Pixel_8 device:y transport_id:1\nZZZ unauthorized usb:1-2 transport_id:2\n* daemon started\n\n"
	ds := parseADBDevices(out)
	if len(ds) != 2 {
		t.Fatalf("got %d devices: %+v", len(ds), ds)
	}
	if ds[0].Name != "Pixel 8" || ds[0].State != "ready" {
		t.Errorf("first: %+v", ds[0])
	}
	if ds[1].State != "unlock the phone" || ds[1].Hint == "" {
		t.Errorf("second: %+v", ds[1])
	}
}

func TestParseStat(t *testing.T) {
	e, ok := parseStat("1234|1700000000|/sdcard/DCIM/Camera/a|b.jpg\r", "/sdcard")
	if !ok || e.Path != "DCIM/Camera/a|b.jpg" || e.Size != 1234 || e.Mtime != 1700000000 {
		t.Errorf("got %+v %v", e, ok)
	}
	for _, bad := range []string{"", "x|y", "1|2|/other/f", "a|2|/sdcard/f", "1|2|/sdcard/"} {
		if _, ok := parseStat(bad, "/sdcard"); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestQuote(t *testing.T) {
	if got := quote("/sdcard/it's"); got != `'/sdcard/it'\''s'` {
		t.Error(got)
	}
}
