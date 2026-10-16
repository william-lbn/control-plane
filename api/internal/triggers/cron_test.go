package triggers

import (
	"testing"
	"time"
)

func TestCronNextUTCNumericGrammarAndCalendar(t *testing.T) {
	tests := []struct{ cron, after, want string }{
		{"* * * * *", "2026-10-16T00:00:00Z", "2026-10-16T00:01:00Z"},
		{"*/15 * * * *", "2026-10-16T00:01:59Z", "2026-10-16T00:15:00Z"},
		{"0 9 * * 1-5", "2026-10-16T09:00:00Z", "2026-10-19T09:00:00Z"},
		{"0,30 * * * *", "2026-10-16T00:30:00Z", "2026-10-16T01:00:00Z"},
		{"15 14 1 * *", "2026-10-16T00:00:00Z", "2026-11-01T14:15:00Z"},
		{"0 0 29 2 *", "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"0 0 * * 7", "2026-10-16T00:00:00Z", "2026-10-18T00:00:00Z"},
		{"0 0 1 * 1", "2026-10-16T00:00:00Z", "2026-10-19T00:00:00Z"},
		{"0 0 */1 * 1", "2026-10-16T00:00:00Z", "2026-10-19T00:00:00Z"},
		{"0 0 *,1 * 1", "2026-10-16T00:00:00Z", "2026-10-19T00:00:00Z"},
		{"0 0 1 1 *", "2026-10-16T00:00:00Z", "2027-01-01T00:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.cron+test.after, func(t *testing.T) {
			cron, err := ParseCron(test.cron)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := time.Parse(time.RFC3339, test.after)
			next, err := cron.Next(after)
			if err != nil || next.Format(time.RFC3339) != test.want {
				t.Fatalf("next=%s err=%v want=%s", next, err, test.want)
			}
			same, err := cron.Next(after.In(time.FixedZone("local", 8*3600)))
			if err != nil || !same.Equal(next) {
				t.Fatal("local time altered UTC schedule")
			}
		})
	}
}

func TestCronRejectsInvalidOrImpossibleSchedules(t *testing.T) {
	for _, text := range []string{"", "* * * * * *", "@daily", "70 * * * *", "0 24 * * *", "0 9 * * MON", "0 0 1 JAN *", "*/0 * * * *", "0 0 30 2 *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 8", "0 0 * * 5-1", "0,,1 * * * *", "1/-2 * * * *", "0 * * * *\n", "0/1/2 * * * *"} {
		if _, err := ParseCron(text); err == nil {
			t.Fatalf("invalid schedule admitted: %q", text)
		}
	}
	var zero Cron
	if _, err := zero.Next(time.Now()); err == nil {
		t.Fatal("zero schedule admitted")
	}
}
