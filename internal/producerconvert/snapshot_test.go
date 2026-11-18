package producerconvert

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestNonDistributionPathsRetainUnsafePathDiagnostic(t *testing.T) {
	for _, selected := range []bool{true, false} {
		t.Run(map[bool]string{true: "selected path", false: "undistributed workflow"}[selected], func(t *testing.T) {
			root, opts := fixture(t)
			field := `.github/workflows/x\y.yml`
			if selected {
				put(t, root, `plugins\orbit/.tessl-plugin/plugin.json`, `{"name":"upstream/orbit","version":"2.3.4"}`, 0o644)
				opts.PackageRoot = filepath.Join(root, `plugins\orbit`)
				field = `plugins\orbit/placeholder`
			} else {
				put(t, root, field, "name: Independent\n", 0o644)
			}
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "unsafe_path" || refusal.Path != field || refusal.Reason != "use a confined relative path" || report.Wrote {
					t.Fatalf("dry=%v: report=%+v, err=%v; want unsafe_path at %q with confined-path guidance", dry, report, err, field)
				}
				absent(t, root, "agent-plugin.yaml")
				absent(t, root, ReceiptPath)
			}
		})
	}
}
