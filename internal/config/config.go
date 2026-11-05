// Package config stores pakket's defaults, like Keka's preferences.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Config holds every default. Keys are the json names.
type Config struct {
	Format          string `json:"format"`
	Level           int    `json:"level"`
	EncryptNames    bool   `json:"encrypt_names"`
	ZipLegacy       bool   `json:"zip_legacy"`
	Solid           bool   `json:"solid"`
	ExcludeMac      bool   `json:"exclude_mac"`
	Separate        bool   `json:"separate"`
	DeleteAfterPack bool   `json:"delete_after_compress"`
	Split           string `json:"split"`
	Checksum        string `json:"checksum"`
	ExportPassword  bool   `json:"export_password"`
	CompressDest    string `json:"compress_dest"`

	ExtractDest  string `json:"extract_dest"`
	Folder       string `json:"folder"`
	DeleteAfterX bool   `json:"delete_after_extract"`
	Quarantine   bool   `json:"quarantine"`
	Encoding     string `json:"encoding"`
	Overwrite    string `json:"overwrite"`
	IgnoreGzName bool   `json:"ignore_gz_name"`
	Nested       bool   `json:"nested"`
	Keychain     bool   `json:"keychain"`
	Notify       bool   `json:"notify"`
	ShowHidden   bool   `json:"show_hidden"`
}

// Default matches Keka's out-of-the-box behaviour where it has one.
func Default() Config {
	return Config{
		Format: "7z", Level: 5, EncryptNames: true, Solid: true, ExcludeMac: true,
		Folder: "smart", Quarantine: true, Encoding: "auto", Overwrite: "rename",
		Nested: true, Keychain: true,
	}
}

// Path is $XDG_CONFIG_HOME/pakket/config.json or ~/.config/pakket/config.json.
func Path() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "pakket", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pakket", "config.json")
}

// Load reads the config, falling back to defaults for anything missing.
func Load() Config {
	c := Default()
	b, err := os.ReadFile(Path())
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// Save writes the config.
func Save(c Config) error {
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(Path(), append(b, '\n'), 0o644)
}

// Keys lists every setting name.
func Keys() []string {
	t := reflect.TypeOf(Config{})
	var ks []string
	for i := 0; i < t.NumField(); i++ {
		ks = append(ks, t.Field(i).Tag.Get("json"))
	}
	sort.Strings(ks)
	return ks
}

func field(c *Config, key string) (reflect.Value, bool) {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("json") == key {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// Get returns one setting as text.
func Get(c Config, key string) (string, error) {
	f, ok := field(&c, key)
	if !ok {
		return "", fmt.Errorf("unknown setting %q", key)
	}
	return fmt.Sprint(f.Interface()), nil
}

// Set changes one setting from text.
func Set(c *Config, key, val string) error {
	f, ok := field(c, key)
	if !ok {
		return fmt.Errorf("unknown setting %q; settings: %s", key, strings.Join(Keys(), ", "))
	}
	switch f.Kind() {
	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("%s needs true or false", key)
		}
		f.SetBool(b)
	case reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("%s needs a number", key)
		}
		f.SetInt(int64(n))
	default:
		f.SetString(val)
	}
	return nil
}
