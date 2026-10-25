package notify

import (
	"strings"
	"testing"
)

func TestSummaryIsTheFirstLineAndPushSized(t *testing.T) {
	why := "Why: XEF.TO closed at 50.44, 1.0% above its 200-day average (49.93): uptrend, so it holds its 33% share.\nOrder: Buy 66 XEF.TO\nRule: trend-filter-200"
	if got := Summary(why); got != "Why: XEF.TO closed at 50.44, 1.0% above its 200-day average (49.93): uptrend, so it holds its 33% share." {
		t.Errorf("summary = %q", got)
	}
	long := "Why: " + strings.Repeat("word ", 200)
	got := Summary(long)
	if len([]rune(got)) > maxSummary+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("long summary not cut: %d runes", len([]rune(got)))
	}
}
