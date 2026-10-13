package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/pelletier/go-toml/v2"
)

// Get returns the value at a dotted key such as backup.vps.schedule,
// postgres.settings.work_mem, clusters.app.size or
// clusters.app.settings.work_mem. Arrays of tables are addressed by their
// name field.
func (c Cluster) Get(key string) (any, error) {
	if strings.TrimSpace(key) == "" {
		return nil, unknownKeyError(key)
	}
	v, err := getIn(reflect.ValueOf(c), strings.Split(key, "."))
	if err != nil {
		return nil, err
	}
	return v.Interface(), nil
}

// Set assigns value to a dotted key. Values parse by the target field's type.
// Immutable keys (immutableHint) are refused.
func (c *Cluster) Set(key, value string) error {
	segs := strings.Split(key, ".")
	if immutableKey(segs) {
		return output.NewError(
			output.CodePrecondition,
			"key "+key+" is immutable after init",
			output.ExitPrecondition,
			immutableHint,
		).WithDetails(map[string]any{"reason": "immutable_key", "key": key})
	}
	if err := setIn(reflect.ValueOf(c).Elem(), segs, value); err != nil {
		return err
	}
	return nil
}

// immutableHint names the keys eru refuses to change.
const immutableHint = "schema, name, nodes.*.wg_address, transport.* and clusters.*.name, .port, .stanza and .follows " +
	"cannot be changed once they exist. A cluster's port and stanza are assigned by eru db create."

// immutableClusterKeys are the [[clusters]] keys fixed at create. follows
// arrives with followers in Phase 6.
var immutableClusterKeys = map[string]bool{"name": true, "port": true, "stanza": true, "follows": true}

// immutableKey reports whether a dotted key names a value eru refuses to change.
func immutableKey(segs []string) bool {
	switch segs[0] {
	case "schema", "name", "transport":
		return true
	case "nodes":
		return len(segs) == 3 && segs[2] == "wg_address"
	case "clusters":
		return len(segs) == 3 && immutableClusterKeys[segs[2]]
	}
	return false
}

// ImmutableHint is the hint for a refused immutable change.
func ImmutableHint() string { return immutableHint }

// ImmutableChanges lists the immutable keys whose value differs between old
// and updated, for whole-file replacements such as config edit.
func ImmutableChanges(old, updated Cluster) []string {
	var changed []string
	if old.Schema != updated.Schema {
		changed = append(changed, "schema")
	}
	if old.ClusterName != updated.ClusterName {
		changed = append(changed, "name")
	}
	if !reflect.DeepEqual(old.Transport, updated.Transport) {
		changed = append(changed, "transport")
	}
	for _, n := range old.Nodes {
		if u, ok := updated.Node(n.Name); ok && u.WGAddress != n.WGAddress {
			changed = append(changed, "nodes."+n.Name+".wg_address")
		}
	}
	for _, cl := range old.Clusters {
		u, ok := updated.ClusterByName(cl.Name)
		if !ok {
			continue
		}
		if u.Port != cl.Port {
			changed = append(changed, "clusters."+cl.Name+".port")
		}
		if u.Stanza != cl.Stanza {
			changed = append(changed, "clusters."+cl.Name+".stanza")
		}
	}
	return changed
}

func getIn(v reflect.Value, segs []string) (reflect.Value, error) {
	for len(segs) > 0 {
		v = reflect.Indirect(v)
		switch v.Kind() {
		case reflect.Struct:
			f, ok := fieldByTag(v, segs[0])
			if !ok {
				return reflect.Value{}, unknownKeyError(strings.Join(segs, "."))
			}
			v = f
			segs = segs[1:]
		case reflect.Slice:
			idx, ok := indexByName(v, segs[0])
			if !ok {
				return reflect.Value{}, unknownKeyError(strings.Join(segs, "."))
			}
			v = v.Index(idx)
			segs = segs[1:]
		case reflect.Map:
			mv := v.MapIndex(reflect.ValueOf(strings.Join(segs, ".")))
			if !mv.IsValid() {
				return reflect.Value{}, unknownKeyError(strings.Join(segs, "."))
			}
			return mv, nil
		default:
			return reflect.Value{}, unknownKeyError(strings.Join(segs, "."))
		}
	}
	return v, nil
}

func setIn(v reflect.Value, segs []string, value string) error {
	for len(segs) > 1 {
		v = reflect.Indirect(v)
		switch v.Kind() {
		case reflect.Struct:
			f, ok := fieldByTag(v, segs[0])
			if !ok {
				return unknownKeyError(strings.Join(segs, "."))
			}
			v = f
			segs = segs[1:]
		case reflect.Slice:
			idx, ok := indexByName(v, segs[0])
			if !ok {
				return unknownKeyError(strings.Join(segs, "."))
			}
			v = v.Index(idx)
			segs = segs[1:]
		case reflect.Map:
			return setMapEntry(v, strings.Join(segs, "."), value)
		default:
			return unknownKeyError(strings.Join(segs, "."))
		}
	}
	v = reflect.Indirect(v)
	switch v.Kind() {
	case reflect.Struct:
		f, ok := fieldByTag(v, segs[0])
		if !ok {
			return unknownKeyError(segs[0])
		}
		return setScalar(f, value)
	case reflect.Map:
		return setMapEntry(v, segs[0], value)
	default:
		return unknownKeyError(segs[0])
	}
}

// fieldByTag returns the struct field whose toml tag is name.
func fieldByTag(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		if base, _, _ := strings.Cut(tag, ","); base == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// indexByName finds the slice element whose name field equals name.
func indexByName(v reflect.Value, name string) (int, bool) {
	if v.Type().Elem().Kind() != reflect.Struct {
		return 0, false
	}
	for i := 0; i < v.Len(); i++ {
		f, ok := fieldByTag(v.Index(i), "name")
		if ok && f.Kind() == reflect.String && f.String() == name {
			return i, true
		}
	}
	return 0, false
}

func setMapEntry(v reflect.Value, key, value string) error {
	if v.IsNil() {
		v.Set(reflect.MakeMap(v.Type()))
	}
	v.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(value))
	return nil
}

// setScalar parses value into field by its kind.
func setScalar(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(value)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return invalidValue(value, "a boolean")
		}
		field.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.Atoi(value)
		if err != nil {
			return invalidValue(value, "an integer")
		}
		field.SetInt(int64(n))
	case reflect.Float64, reflect.Float32:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return invalidValue(value, "a number")
		}
		field.SetFloat(f)
	case reflect.Slice:
		list, err := parseStringList(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(list))
	default:
		return output.NewError(output.CodeValidation, "this key cannot be set from the command line", output.ExitConfig,
			"Edit cluster.toml with eru config edit instead.")
	}
	return nil
}

// parseStringList accepts a comma list or a TOML array literal.
func parseStringList(value string) ([]string, error) {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "[") {
		var wrap struct {
			V []string `toml:"v"`
		}
		if err := toml.Unmarshal([]byte("v = "+trimmed), &wrap); err != nil {
			return nil, invalidValue(value, "a list")
		}
		if wrap.V == nil {
			return []string{}, nil
		}
		return wrap.V, nil
	}
	if trimmed == "" {
		return []string{}, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func unknownKeyError(key string) *output.CLIError {
	return output.NewError(
		output.CodeConfig,
		"unknown key "+key,
		output.ExitConfig,
		"Run eru config show to see the keys, or eru config edit for a full view.",
	).WithDetails(map[string]any{"key": key})
}

func invalidValue(value, want string) *output.CLIError {
	return output.NewError(
		output.CodeValidation,
		fmt.Sprintf("%q is not %s", value, want),
		output.ExitConfig,
		"",
	)
}
