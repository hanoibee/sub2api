package config

import "testing"

func TestRequestArchiveConfiguration(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.RequestArchive.Enabled || cfg.RequestArchive.Directory != "" || cfg.RequestArchive.ProviderCode != "" {
			t.Fatalf("defaults: %+v", cfg.RequestArchive)
		}
	})
	t.Run("environment", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv("REQUEST_ARCHIVE_ENABLED", "false")
		t.Setenv("REQUEST_ARCHIVE_DIRECTORY", "/tmp/request-archive-example")
		t.Setenv("REQUEST_ARCHIVE_PROVIDER_CODE", "custom-code")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RequestArchive.Enabled || cfg.RequestArchive.Directory != "/tmp/request-archive-example" || cfg.RequestArchive.ProviderCode != "custom-code" {
			t.Fatalf("config: %+v", cfg.RequestArchive)
		}
	})
}
