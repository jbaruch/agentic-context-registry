package freshness

import "testing"

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stored   string
		flag     string
		explicit bool
		want     Policy
		persist  bool
	}{
		{name: "explicit flag", stored: "outdated", flag: "install", explicit: true, want: PolicyInstall, persist: true},
		{name: "stored", stored: "none", flag: "outdated", want: PolicyNone},
		{name: "default", want: PolicyOutdated, persist: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, persist := Resolve(test.stored, test.flag, test.explicit)
			if got != test.want || persist != test.persist {
				t.Fatalf("Resolve() = %q, %t, want %q, %t", got, persist, test.want, test.persist)
			}
		})
	}
}
