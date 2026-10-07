// Package cast loads cast.json, the single source of truth for who works in
// this kitchen. The engine reads the same file for spawn points; the brain
// reads it for personas, forms of address, and which model tier each character
// thinks on. Adding a cook is a config edit, not a code change.
package cast

import (
	"encoding/json"
	"fmt"
	"os"
)

type Character struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Role      string            `json:"role"`
	Spawn     string            `json:"spawn"`
	Color     string            `json:"color"`
	Tier      string            `json:"tier"` // "primary" (cloud model) or "background" (local/cheap)
	Specialty string            `json:"specialty"`
	Persona   string            `json:"persona"`
	Addresses map[string]string `json:"addresses"` // other character ID -> what this one calls them
}

type Cast struct {
	Restaurant string            `json:"restaurant"`
	Stations   map[string]string `json:"stations"`
	Characters []Character       `json:"characters"`
}

func Load(path string) (*Cast, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read cast file %s: %w", path, err)
	}
	var c Cast
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("cast file %s is not valid JSON: %w", path, err)
	}
	if len(c.Characters) == 0 {
		return nil, fmt.Errorf("cast file %s has no characters", path)
	}
	return &c, nil
}

func (c *Cast) Get(id string) (Character, bool) {
	for _, ch := range c.Characters {
		if ch.ID == id {
			return ch, true
		}
	}
	return Character{}, false
}

func (c *Cast) IDs() []string {
	ids := make([]string, 0, len(c.Characters))
	for _, ch := range c.Characters {
		ids = append(ids, ch.ID)
	}
	return ids
}

// Calls returns what `ch` calls the character with id `other`.
func (c *Cast) Calls(ch Character, other string) string {
	if name, ok := ch.Addresses[other]; ok {
		return name
	}
	if o, ok := c.Get(other); ok {
		return o.Name
	}
	if def, ok := ch.Addresses["default"]; ok {
		return def
	}
	return other
}

// Roster is the "who else is here" block injected into every prompt, written
// from this character's point of view so the forms of address are theirs.
func (c *Cast) Roster(ch Character) string {
	out := ""
	for _, other := range c.Characters {
		if other.ID == ch.ID {
			continue
		}
		out += fmt.Sprintf("- %s (id: %s), %s. You call them %q. Specialty: %s\n",
			other.Name, other.ID, other.Role, c.Calls(ch, other.ID), other.Specialty)
	}
	return out
}
