package reflect

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Struct builds a new struct using the data in `object`, as long as `object`
// is a `tftypes.Object`. It will take the struct type from `target`, which
// must be a struct type.
//
// The properties on `target` must be tagged with a "tfsdk" label containing
// the field name to map to that property. Every property must be tagged, and
// every property must be present in the type of `object`, and all the
// attributes in the type of `object` must have a corresponding property.
// Properties that don't map to object attributes must have a `tfsdk:"-"` tag,
// explicitly defining them as not part of the object. This is to catch typos
// and other mistakes early.
//
// Struct is meant to be called from Into, not directly.
func Struct(ctx context.Context, typ attr.Type, object tftypes.Value, target reflect.Value, opts Options, path path.Path) (reflect.Value, diag.Diagnostics) {
	var diags diag.Diagnostics

	// this only works with object values, so make sure that constraint is
	// met
	if target.Kind() != reflect.Struct {
		diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
			Val:        object,
			TargetType: target.Type(),
			Err:        fmt.Errorf("expected a struct type, got %s", target.Type()),
		}))
		return target, diags
	}
	if !object.Type().Is(tftypes.Object{}) {
		diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
			Val:        object,
			TargetType: target.Type(),
			Err:        fmt.Errorf("cannot reflect %s into a struct, must be an object", object.Type().String()),
		}))
		return target, diags
	}
	attrsType, ok := typ.(attr.TypeWithAttributeTypes)
	if !ok {
		diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
			Val:        object,
			TargetType: target.Type(),
			Err:        fmt.Errorf("cannot reflect object using type information provided by %T, %T must be an attr.TypeWithAttributeTypes", typ, typ),
		}))
		return target, diags
	}

	// collect a map of fields that are in the object passed in
	var objectFields map[string]tftypes.Value
	err := object.As(&objectFields)
	if err != nil {
		diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
			Val:        object,
			TargetType: target.Type(),
			Err:        err,
		}))
		return target, diags
	}

	// collect a map of fields that are defined in the tags of the struct
	// passed in
	targetFields := typeFields(target.Type())

	// we require an exact, 1:1 match of these fields to avoid typos
	// leading to surprises, so let's ensure they have the exact same
	// fields defined
	var objectMissing, targetMissing []string
	for field := range targetFields.nameIndex {
		if _, ok := objectFields[field]; !ok {
			objectMissing = append(objectMissing, field)
		}
	}
	for field := range objectFields {
		if _, ok := targetFields.nameIndex[field]; !ok {
			targetMissing = append(targetMissing, field)
		}
	}
	if len(objectMissing) > 0 || len(targetMissing) > 0 {
		var missing []string
		if len(objectMissing) > 0 {
			missing = append(missing, fmt.Sprintf("Struct defines fields not found in object: %s.", commaSeparatedString(objectMissing)))
		}
		if len(targetMissing) > 0 {
			missing = append(missing, fmt.Sprintf("Object defines fields not found in struct: %s.", commaSeparatedString(targetMissing)))
		}
		diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
			Val:        object,
			TargetType: target.Type(),
			Err:        fmt.Errorf("mismatch between struct and object: %s", strings.Join(missing, " ")),
		}))
		return target, diags
	}

	attrTypes := attrsType.AttributeTypes()

	// now that we know they match perfectly, fill the struct with the
	// values in the object
	result := reflect.New(target.Type()).Elem()
	for _, field := range targetFields.list {
		attrType, ok := attrTypes[field.name]
		if !ok {
			diags.Append(diag.WithPath(path, DiagIntoIncompatibleType{
				Val:        object,
				TargetType: target.Type(),
				Err:        fmt.Errorf("could not find type information for attribute in supplied attr.Type %T", typ),
			}))
			return target, diags
		}

		structField := fieldByIndex(result, field.index)
		fieldVal, fieldValDiags := BuildValue(ctx, attrType, objectFields[field.name], structField, opts, path.AtName(field.name))
		diags.Append(fieldValDiags...)

		if diags.HasError() {
			return target, diags
		}
		structField.Set(fieldVal)
	}
	return result, diags
}

// FromStruct builds an attr.Value as produced by `typ` from the data in `val`.
// `val` must be a struct type, and must have all its properties tagged and be
// a 1:1 match with the attributes reported by `typ`. FromStruct will recurse
// into FromValue for each attribute, using the type of the attribute as
// reported by `typ`.
//
// It is meant to be called through FromValue, not directly.
func FromStruct(ctx context.Context, typ attr.TypeWithAttributeTypes, val reflect.Value, path path.Path) (attr.Value, diag.Diagnostics) {
	var diags diag.Diagnostics
	objTypes := map[string]tftypes.Type{}
	objValues := map[string]tftypes.Value{}

	// collect a map of fields that are defined in the tags of the struct
	// passed in
	valFields := typeFields(val.Type())

	attrTypes := typ.AttributeTypes()
	for _, field := range valFields.list {
		path := path.AtName(field.name)
		fieldValue := fieldByIndex(val, field.index)

		attrVal, attrValDiags := FromValue(ctx, attrTypes[field.name], fieldValue.Interface(), path)
		diags.Append(attrValDiags...)

		if diags.HasError() {
			return nil, diags
		}

		attrType, ok := attrTypes[field.name]
		if !ok || attrType == nil {
			err := fmt.Errorf("couldn't find type information for attribute at %s in supplied attr.Type %T", path, typ)
			diags.AddAttributeError(
				path,
				"Value Conversion Error",
				"An unexpected error was encountered trying to convert from struct value. This is always an error in the provider. Please report the following to the provider developer:\n\n"+err.Error(),
			)
			return nil, diags
		}

		objTypes[field.name] = attrType.TerraformType(ctx)

		tfObjVal, err := attrVal.ToTerraformValue(ctx)
		if err != nil {
			return nil, append(diags, toTerraformValueErrorDiag(err, path))
		}

		if typeWithValidate, ok := typ.(xattr.TypeWithValidate); ok {
			diags.Append(typeWithValidate.Validate(ctx, tfObjVal, path)...)

			if diags.HasError() {
				return nil, diags
			}
		}

		objValues[field.name] = tfObjVal
	}

	tfVal := tftypes.NewValue(tftypes.Object{
		AttributeTypes: objTypes,
	}, objValues)

	if typeWithValidate, ok := typ.(xattr.TypeWithValidate); ok {
		diags.Append(typeWithValidate.Validate(ctx, tfVal, path)...)

		if diags.HasError() {
			return nil, diags
		}
	}

	retType := typ.WithAttributeTypes(attrTypes)
	ret, err := retType.ValueFromTerraform(ctx, tfVal)
	if err != nil {
		return nil, append(diags, valueFromTerraformErrorDiag(err, path))
	}

	return ret, diags
}
