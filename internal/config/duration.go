package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a human-readable YAML string
// such as "10m" or "750ms". yaml.v3 will happily decode a bare integer into a
// time.Duration as nanoseconds, which silently turns `ttl: 10` into 10ns — a bug
// that would be invisible until the cache appeared not to work. Requiring a
// string makes that mistake impossible.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a quoted string like \"10m\": %w", err)
	}
	return d.parse(s)
}

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(v)
	return nil
}
