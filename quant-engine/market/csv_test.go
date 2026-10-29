package market

import (
	"strings"
	"testing"
)

func TestReadCSV(t *testing.T) {
	good := csvHeader + "\n2026-01-01T00:00:00Z,1,2,0.5,1.5,10\n2026-01-01T01:00:00Z,1.5,2,1,2,20\n"
	cs, err := ReadCSV(strings.NewReader(good))
	if err != nil || len(cs) != 2 || cs[1].Close != 2 || cs[0].Volume != 10 {
		t.Fatalf("ReadCSV = %+v, %v", cs, err)
	}
	for name, bad := range map[string]string{
		"header":    "t,o,h,l,c,v\n",
		"time":      csvHeader + "\nyesterday,1,1,1,1,1\n",
		"number":    csvHeader + "\n2026-01-01T00:00:00Z,1,x,1,1,1\n",
		"order":     csvHeader + "\n2026-01-01T01:00:00Z,1,1,1,1,1\n2026-01-01T00:00:00Z,1,1,1,1,1\n",
		"short row": csvHeader + "\n2026-01-01T00:00:00Z,1,1\n",
	} {
		if _, err := ReadCSV(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted bad candles", name)
		}
	}
}
