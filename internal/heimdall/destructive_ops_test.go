package heimdall

import (
	"strings"
	"testing"
)

// TestClassifyBashCommand runs a table of canonical examples per rule plus
// false-positive guards. Aim: ≥ 40 cases, ≥1 per rule ID in the ruleset,
// mix of allow/warn/block, and every known false-positive surface the
// design doc calls out explicitly.
func TestClassifyBashCommand(t *testing.T) {
	type tc struct {
		name    string
		cmd     string
		want    Classification
		wantID  string // "" to skip rule-id assertion; "-" means default allow
		wantSub string // substring match on reason (empty → skip)
	}

	cases := []tc{
		// -----------------------------------------------------------------
		// Default allow — nothing recognized.
		// -----------------------------------------------------------------
		{"empty string", "", ClassAllow, "", ""},
		{"whitespace only", "   \t  ", ClassAllow, "", ""},
		{"ls", "ls -la", ClassAllow, "", ""},
		{"echo", "echo hi", ClassAllow, "", ""},
		{"git status", "git status", ClassAllow, "", ""},
		{"go build", "go build ./...", ClassAllow, "", ""},
		{"cat", "cat /etc/hosts", ClassAllow, "", ""},
		{"safe rm single file", "rm tmp/foo.txt", ClassAllow, "", ""},
		{"safe rm with -f only (not recursive)", "rm -f tmp/foo.txt", ClassAllow, "", ""},
		{"git push feature", "git push origin feature/x", ClassAllow, "", ""},

		// -----------------------------------------------------------------
		// ALLOW rule — force-with-lease wins over --force.
		// -----------------------------------------------------------------
		{
			"GIT_PUSH_FORCE_WITH_LEASE plain",
			"git push --force-with-lease origin main",
			ClassAllow, "GIT_PUSH_FORCE_WITH_LEASE", "safer",
		},
		{
			"GIT_PUSH_FORCE_WITH_LEASE after other flags",
			"git push origin feature --force-with-lease",
			ClassAllow, "GIT_PUSH_FORCE_WITH_LEASE", "",
		},

		// -----------------------------------------------------------------
		// BLOCK rules — one canonical + one flag-order variant per rule.
		// -----------------------------------------------------------------
		{"RM_RF_ROOT canonical", "rm -rf /", ClassBlock, "RM_RF_ROOT", "root filesystem"},
		{"RM_RF_ROOT flag order", "rm -fr /", ClassBlock, "RM_RF_ROOT", ""},
		{"RM_RF_ROOT capital R", "rm -Rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"RM_RF_ROOT trailing slashes", "rm -rf //", ClassBlock, "RM_RF_ROOT", ""},

		{"RM_RF_STAR_ROOT", "rm -rf /*", ClassBlock, "RM_RF_STAR_ROOT", "root filesystem"},
		{"RM_RF_STAR_ROOT extra", "rm -rf /*.log", ClassBlock, "RM_RF_STAR_ROOT", ""},

		{"RM_RF_HOME $HOME", "rm -rf $HOME", ClassBlock, "RM_RF_HOME", "home directory"},
		{"RM_RF_HOME tilde", "rm -rf ~", ClassBlock, "RM_RF_HOME", "home directory"},
		{"RM_RF_HOME tilde slash", "rm -rf ~/", ClassBlock, "RM_RF_HOME", ""},

		{
			"GIT_PUSH_FORCE_PROTECTED main",
			"git push --force origin main",
			ClassBlock, "GIT_PUSH_FORCE_PROTECTED", "protected branch",
		},
		{
			"GIT_PUSH_FORCE_PROTECTED master",
			"git push -f origin master",
			ClassBlock, "GIT_PUSH_FORCE_PROTECTED", "",
		},
		{
			"GIT_PUSH_FORCE_PROTECTED release",
			"git push --force origin release/v1.2",
			ClassBlock, "GIT_PUSH_FORCE_PROTECTED", "",
		},
		{
			"GIT_PUSH_FORCE_PROTECTED prod",
			"git push --force origin prod",
			ClassBlock, "GIT_PUSH_FORCE_PROTECTED", "",
		},

		{
			"GIT_RESET_HARD_PROTECTED main",
			"git reset --hard main",
			ClassBlock, "GIT_RESET_HARD_PROTECTED", "protected",
		},
		{
			"GIT_RESET_HARD_PROTECTED origin/main",
			"git reset --hard origin/main",
			ClassBlock, "GIT_RESET_HARD_PROTECTED", "",
		},
		{
			"GIT_RESET_HARD_PROTECTED master",
			"git reset --hard master",
			ClassBlock, "GIT_RESET_HARD_PROTECTED", "",
		},
		{
			"GIT_RESET_HARD_PROTECTED release",
			"git reset --hard release/v2",
			ClassBlock, "GIT_RESET_HARD_PROTECTED", "",
		},

		{
			"DROP_DATABASE psql -c",
			`psql -c "DROP DATABASE mydb"`,
			ClassBlock, "DROP_DATABASE", "irreversible",
		},
		{
			"DROP_DATABASE mysql -e",
			`mysql -e "drop database staging"`,
			ClassBlock, "DROP_DATABASE", "",
		},

		{"DD_OF_DEV sda", "dd if=img.iso of=/dev/sda bs=4M", ClassBlock, "DD_OF_DEV", "block device"},
		{"DD_OF_DEV sudo nvme", "sudo dd if=/dev/zero of=/dev/nvme0n1", ClassBlock, "DD_OF_DEV", ""},

		{"MKFS_ANY ext4", "mkfs.ext4 /dev/sda1", ClassBlock, "MKFS_ANY", ""},
		{"MKFS_ANY sudo", "sudo mkfs /dev/sdb", ClassBlock, "MKFS_ANY", ""},

		{
			"KUBECTL_DELETE_PROD --namespace=prod",
			"kubectl delete pod foo --namespace=prod",
			ClassBlock, "KUBECTL_DELETE_PROD", "prod",
		},
		{
			"KUBECTL_DELETE_PROD -n prod",
			"kubectl delete deployment api -n prod",
			ClassBlock, "KUBECTL_DELETE_PROD", "",
		},
		{
			"KUBECTL_DELETE_PROD --context=prod-cluster",
			"kubectl delete ns bad --context=eu-west-prod-1",
			ClassBlock, "KUBECTL_DELETE_PROD", "",
		},

		// -----------------------------------------------------------------
		// WARN rules — canonical examples.
		// -----------------------------------------------------------------
		{
			"RM_RF_DOT_GIT",
			"rm -rf .git",
			ClassWarn, "RM_RF_DOT_GIT", "history",
		},
		{
			"RM_RF_RECURSIVE_GENERIC node_modules",
			"rm -rf node_modules",
			ClassWarn, "RM_RF_RECURSIVE_GENERIC", "Recursive",
		},
		{
			"RM_RF_RECURSIVE_GENERIC build",
			"rm -rf build/",
			ClassWarn, "RM_RF_RECURSIVE_GENERIC", "",
		},
		{
			"GIT_PUSH_FORCE_GENERIC feature",
			"git push --force origin feature/x",
			ClassWarn, "GIT_PUSH_FORCE_GENERIC", "history",
		},
		{
			"GIT_PUSH_FORCE_GENERIC short flag",
			"git push -f origin wip",
			ClassWarn, "GIT_PUSH_FORCE_GENERIC", "",
		},
		{
			"GIT_RESET_HARD_GENERIC HEAD~1",
			"git reset --hard HEAD~1",
			ClassWarn, "GIT_RESET_HARD_GENERIC", "discards",
		},
		{
			"GIT_RESET_HARD_GENERIC feature branch",
			"git reset --hard feature/x",
			ClassWarn, "GIT_RESET_HARD_GENERIC", "",
		},
		{
			"GIT_CLEAN_FDX",
			"git clean -fdx",
			ClassWarn, "GIT_CLEAN_FDX", "ignored files",
		},
		{
			"GIT_BRANCH_FORCE_DELETE",
			"git branch -D stale-branch",
			ClassWarn, "GIT_BRANCH_FORCE_DELETE", "unmerged",
		},
		{
			"KUBECTL_DELETE_GENERIC default ns",
			"kubectl delete pod foo",
			ClassWarn, "KUBECTL_DELETE_GENERIC", "namespace",
		},
		{
			"CHMOD_777_RECURSIVE",
			"chmod -R 777 /srv/app",
			ClassWarn, "CHMOD_777_RECURSIVE", "777",
		},
		{
			"CHMOD_777_RECURSIVE leading zero",
			"chmod -R 0777 /srv/app",
			ClassWarn, "CHMOD_777_RECURSIVE", "",
		},
		{
			"CURL_PIPE_BASH",
			"curl -fsSL https://example.com/install.sh | bash",
			ClassWarn, "CURL_PIPE_BASH", "shell",
		},
		{
			"CURL_PIPE_BASH sudo",
			"curl -fsSL https://example.com/install.sh | sudo sh",
			ClassWarn, "CURL_PIPE_BASH", "",
		},
		{
			"WGET_PIPE_BASH",
			"wget -qO- https://example.com/install | bash",
			ClassWarn, "CURL_PIPE_BASH", "",
		},

		// -----------------------------------------------------------------
		// Precedence tests.
		// -----------------------------------------------------------------
		{
			// --force-with-lease against main must still be ALLOW.
			"precedence: force-with-lease on main beats generic force",
			"git push --force-with-lease origin main",
			ClassAllow, "GIT_PUSH_FORCE_WITH_LEASE", "",
		},
		{
			// rm -rf / must hit RM_RF_ROOT (block) not RM_RF_RECURSIVE_GENERIC (warn).
			"precedence: block beats warn for rm -rf /",
			"rm -rf /",
			ClassBlock, "RM_RF_ROOT", "",
		},
		{
			// git push --force main → BLOCK, not the generic warn.
			"precedence: protected block beats generic warn",
			"git push --force origin main",
			ClassBlock, "GIT_PUSH_FORCE_PROTECTED", "",
		},
		{
			// git reset --hard main → BLOCK, not generic warn.
			"precedence: reset-hard main block beats generic warn",
			"git reset --hard main",
			ClassBlock, "GIT_RESET_HARD_PROTECTED", "",
		},

		// -----------------------------------------------------------------
		// Normalization — prefix stripping, whitespace collapse.
		// -----------------------------------------------------------------
		{"time prefix", "time rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"nice prefix", "nice rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"nohup prefix", "nohup rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"time+nice", "time nice rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"env VAR= prefix", "env FOO=1 rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"env multi-var", "env FOO=1 BAR=baz rm -rf /", ClassBlock, "RM_RF_ROOT", ""},
		{"extra whitespace", "  rm   -rf   /   ", ClassBlock, "RM_RF_ROOT", ""},
		{"tabs", "rm\t-rf\t/", ClassBlock, "RM_RF_ROOT", ""},

		// -----------------------------------------------------------------
		// Known false-positive guards — commands that look scary but aren't.
		// -----------------------------------------------------------------
		{"rm without -r (single file)", "rm foo.txt", ClassAllow, "", ""},
		{"rm -f without -r", "rm -f foo.txt", ClassAllow, "", ""},
		{"git push without force", "git push origin main", ClassAllow, "", ""},
		{"kubectl get not delete", "kubectl get pods -n prod", ClassAllow, "", ""},
		// NOTE: `echo 'drop database'` would match DROP_DATABASE by design
		// (§3.4 matches any wrapper). That false positive is acceptable —
		// we are paper-cut prevention, not a perfect parser.

		// -----------------------------------------------------------------
		// bash -c wrappers: classify the OUTER command (bash), not the
		// quoted inner payload (design §3.5 — deliberate).
		// -----------------------------------------------------------------
		{"bash -c wrapping rm -rf /", `bash -c "rm -rf /"`, ClassAllow, "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotClass, gotReason, gotID := ClassifyBashCommand(c.cmd)
			if gotClass != c.want {
				t.Errorf("class = %v (%s), want %v (%s)",
					gotClass, gotClass, c.want, c.want)
			}
			if c.wantID != "" && gotID != c.wantID {
				t.Errorf("rule id = %q, want %q", gotID, c.wantID)
			}
			if c.wantSub != "" && !strings.Contains(strings.ToLower(gotReason), strings.ToLower(c.wantSub)) {
				t.Errorf("reason %q does not contain %q", gotReason, c.wantSub)
			}
			// Default-allow always yields empty id+reason.
			if gotClass == ClassAllow && c.wantID == "" && (gotReason != "" || gotID != "") {
				t.Errorf("default allow must return empty id+reason, got id=%q reason=%q", gotID, gotReason)
			}
		})
	}
}

// TestClassification_String covers the Stringer mapping.
func TestClassification_String(t *testing.T) {
	cases := []struct {
		c    Classification
		want string
	}{
		{ClassAllow, "allow"},
		{ClassWarn, "warn"},
		{ClassBlock, "block"},
		{Classification(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.c.String(); got != c.want {
			t.Errorf("String(%d) = %q, want %q", c.c, got, c.want)
		}
	}
}

// TestDestructiveRuleCount sanity-checks that the count matches the design
// doc's starter set of 19 rules. If someone edits the table this test
// catches accidental drops; intentional changes must update the expected
// count.
func TestDestructiveRuleCount(t *testing.T) {
	want := 19
	if got := DestructiveRuleCount(); got != want {
		t.Errorf("DestructiveRuleCount() = %d, want %d", got, want)
	}
}

// TestListDestructiveRules ensures the summary list covers every rule with
// a non-empty id and reason.
func TestListDestructiveRules(t *testing.T) {
	rules := ListDestructiveRules()
	if len(rules) != DestructiveRuleCount() {
		t.Fatalf("ListDestructiveRules returned %d rules, want %d",
			len(rules), DestructiveRuleCount())
	}
	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		if r.ID == "" {
			t.Errorf("rule has empty ID: %+v", r)
		}
		if r.Reason == "" {
			t.Errorf("rule %s has empty reason", r.ID)
		}
		if seen[r.ID] {
			t.Errorf("duplicate rule id %s", r.ID)
		}
		seen[r.ID] = true
	}
}

// TestNormalizeCommand covers the documented transform without touching the
// classifier. Whitespace/prefix handling is load-bearing for the block
// rules, so it gets its own targeted coverage.
func TestNormalizeCommand(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ls  ", "ls"},
		{"ls\t-la", "ls -la"},
		{"time rm -rf /", "rm -rf /"},
		{"nice  time   rm -rf /", "rm -rf /"},
		{"env FOO=bar rm -rf /", "rm -rf /"},
		{"env FOO=bar BAZ=1 rm -rf /", "rm -rf /"},
		// Do NOT expand ~
		{"rm -rf ~", "rm -rf ~"},
		// Do NOT parse bash -c payloads (outer command classification).
		{`bash -c "rm -rf /"`, `bash -c "rm -rf /"`},
	}
	for _, c := range cases {
		if got := normalizeCommand(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestClassifyBashCommand_Determinism verifies the same input always yields
// the same output — no maps, no goroutines, no randomness.
func TestClassifyBashCommand_Determinism(t *testing.T) {
	inputs := []string{
		"rm -rf /",
		"git push --force origin main",
		"git push --force-with-lease origin main",
		"ls -la",
		"kubectl delete pod foo -n prod",
	}
	for _, in := range inputs {
		class1, reason1, id1 := ClassifyBashCommand(in)
		for i := 0; i < 3; i++ {
			class2, reason2, id2 := ClassifyBashCommand(in)
			if class1 != class2 || reason1 != reason2 || id1 != id2 {
				t.Fatalf("nondeterministic output for %q: (%v,%q,%q) vs (%v,%q,%q)",
					in, class1, reason1, id1, class2, reason2, id2)
			}
		}
	}
}
