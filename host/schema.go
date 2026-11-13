package host

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/types/known/structpb"
)

func validateJSON(schema *structpb.Value, instance []byte) error {
	raw, err := schema.MarshalJSON()
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(jsonschema.SchemeURLLoader{})
	if err := c.AddResource("mem://config.json", doc); err != nil {
		return err
	}
	sch, err := c.Compile("mem://config.json")
	if err != nil {
		return fmt.Errorf("invalid config_schema: %w", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

func satisfies(version, constraint string) error {
	if constraint == "" {
		return nil
	}
	have := "v" + strings.TrimPrefix(version, "v")
	if !semver.IsValid(have) {
		return fmt.Errorf("plugin version %q is not semver, constraint %q cannot hold", version, constraint)
	}
	for _, term := range strings.FieldsFunc(constraint, func(r rune) bool { return r == ' ' || r == ',' }) {
		rest := strings.TrimLeft(term, "<>=")
		op := term[:len(term)-len(rest)]
		want := "v" + strings.TrimPrefix(rest, "v")
		if !semver.IsValid(want) {
			return fmt.Errorf("version constraint term %q is not valid", term)
		}
		cmp := semver.Compare(have, want)
		ok := map[string]bool{">=": cmp >= 0, ">": cmp > 0, "<=": cmp <= 0, "<": cmp < 0, "=": cmp == 0, "": cmp == 0}
		held, known := ok[op]
		if !known {
			return fmt.Errorf("version constraint term %q has an unknown operator", term)
		}
		if !held {
			return fmt.Errorf("plugin version %s does not satisfy %q", version, constraint)
		}
	}
	return nil
}
