package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatrixConfig_ValidateReportsAllMissing(t *testing.T) {
	t.Parallel()

	err := (MatrixConfig{}).validate()
	if err == nil {
		t.Fatal("expected error for empty MatrixConfig")
	}

	msg := err.Error()
	for _, want := range []string{
		"OPENCROW_MATRIX_HOMESERVER",
		"OPENCROW_MATRIX_USER_ID",
		"OPENCROW_MATRIX_ACCESS_TOKEN",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// testEnv returns a getenv function backed by a map.
func testEnv(m map[string]string) func(string) string {
	return func(key string) string {
		return m[key]
	}
}

// baseMatrixEnv returns the minimum environment needed for Matrix.
func baseMatrixEnv() map[string]string {
	// #nosec G101 -- these are non-functional test-only Matrix credentials.
	return map[string]string{
		"OPENCROW_MATRIX_HOMESERVER":   "https://matrix.example.com",
		"OPENCROW_MATRIX_USER_ID":      "@bot:example.com",
		"OPENCROW_MATRIX_ACCESS_TOKEN": "syt_test_token",
	}
}

func TestLoadConfig_LegacyMatrixBackendAccepted(t *testing.T) {
	t.Parallel()

	env := baseMatrixEnv()
	env["OPENCROW_BACKEND"] = "matrix"

	if _, err := loadConfig(testEnv(env)); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
}

func TestLoadConfig_RejectsNonMatrixBackend(t *testing.T) {
	t.Parallel()

	env := baseMatrixEnv()
	env["OPENCROW_BACKEND"] = "telegram"

	_, err := loadConfig(testEnv(env))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "only supports Matrix") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMatrixConfig_AllowedUsersParsing(t *testing.T) {
	t.Parallel()

	env := baseMatrixEnv()
	env["OPENCROW_ALLOWED_USERS"] = " @alice:example.com, @bob:example.com "

	cfg, err := loadConfig(testEnv(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	for _, userID := range []string{"@alice:example.com", "@bob:example.com"} {
		if _, ok := cfg.Matrix.AllowedUsers[userID]; !ok {
			t.Errorf("allowed user %q missing from %v", userID, cfg.Matrix.AllowedUsers)
		}
	}
}

func TestDiscoverSkills_Symlinks(t *testing.T) {
	t.Parallel()

	// Create a target directory with SKILL.md
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a skills dir with a symlink to the target
	skillsDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(skillsDir, "my-skill")); err != nil {
		t.Fatal(err)
	}

	skills := discoverSkills(skillsDir)
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1: %v", len(skills), skills)
	}

	want := filepath.Join(skillsDir, "my-skill")
	if skills[0] != want {
		t.Errorf("skill path = %q, want %q", skills[0], want)
	}
}

func TestLoadConfig_GroupTriggerRegex(t *testing.T) {
	t.Parallel()

	t.Run("unset", func(t *testing.T) {
		t.Parallel()

		cfg, err := loadConfig(testEnv(baseMatrixEnv()))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}

		if cfg.GroupTriggerRegex != nil {
			t.Errorf("GroupTriggerRegex = %v, want nil", cfg.GroupTriggerRegex)
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		env := baseMatrixEnv()
		env["OPENCROW_GROUP_TRIGGER_REGEX"] = `(?i)\b(barnaby|barn)\b`

		cfg, err := loadConfig(testEnv(env))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}

		if cfg.GroupTriggerRegex == nil {
			t.Fatal("GroupTriggerRegex is nil, want compiled regex")
		}

		if !cfg.GroupTriggerRegex.MatchString("Hey Barn, what's up?") {
			t.Error("regex did not match expected string")
		}

		if cfg.GroupTriggerRegex.MatchString("Hello everyone") {
			t.Error("regex matched unexpected string")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()

		env := baseMatrixEnv()
		env["OPENCROW_GROUP_TRIGGER_REGEX"] = `[unclosed`

		_, err := loadConfig(testEnv(env))
		if err == nil {
			t.Fatal("expected error for invalid regex, got nil")
		}

		if !strings.Contains(err.Error(), "invalid OPENCROW_GROUP_TRIGGER_REGEX") {
			t.Errorf("error %q does not mention OPENCROW_GROUP_TRIGGER_REGEX", err.Error())
		}
	})
}
