package converter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	protovalidate "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"github.com/invopop/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"
	"github.com/xeipuuv/gojsonschema"
	"google.golang.org/protobuf/proto"
	descriptor "google.golang.org/protobuf/types/descriptorpb"

	protoc_gen_jsonschema "github.com/chrusty/protoc-gen-jsonschema"
	protoc_gen_validate "github.com/envoyproxy/protoc-gen-validate/validate"
)

var (
	globalPkg = newProtoPackage(nil, "")

	wellKnownTypes = map[string]bool{
		"BoolValue":   true,
		"BytesValue":  true,
		"DoubleValue": true,
		"Duration":    true,
		"FloatValue":  true,
		"Int32Value":  true,
		"Int64Value":  true,
		"ListValue":   true,
		"StringValue": true,
		"Struct":      true,
		"UInt32Value": true,
		"UInt64Value": true,
		"Value":       true,
	}
)

func (c *Converter) registerEnum(pkgName string, enum *descriptor.EnumDescriptorProto) {
	pkg := globalPkg
	if pkgName != "" {
		for _, node := range strings.Split(pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}
			child, ok := pkg.children[node]
			if !ok {
				child = newProtoPackage(pkg, node)
				pkg.children[node] = child
			}
			pkg = child
		}
	}
	pkg.enums[enum.GetName()] = enum
}

func (c *Converter) registerType(pkgName string, msgDesc *descriptor.DescriptorProto) {
	pkg := globalPkg
	if pkgName != "" {
		for _, node := range strings.Split(pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}
			child, ok := pkg.children[node]
			if !ok {
				child = newProtoPackage(pkg, node)
				pkg.children[node] = child
			}
			pkg = child
		}
	}
	pkg.types[msgDesc.GetName()] = msgDesc
}

// Convert a proto "field" (essentially a type-switch with some recursion):
func (c *Converter) convertField(curPkg *ProtoPackage, desc *descriptor.FieldDescriptorProto, msgDesc *descriptor.DescriptorProto, duplicatedMessages map[*descriptor.DescriptorProto]string, messageFlags ConverterFlags) (*jsonschema.Schema, error) {

	// Prepare a new jsonschema.Schema for our eventual return value:
	jsonSchemaType := &jsonschema.Schema{}

	// Generate a description from src comments (if available)
	if src := c.sourceInfo.GetField(desc); src != nil {
		jsonSchemaType.Title, jsonSchemaType.Description = c.formatTitleAndDescription(nil, src)
	}

	// Helper to get the concrete (non-null) schema when nullable oneOf is used
	getTargetSchema := func(s *jsonschema.Schema) *jsonschema.Schema {
		if len(s.OneOf) > 0 {
			for _, sub := range s.OneOf {
				if sub != nil && sub.Type != gojsonschema.TYPE_NULL {
					return sub
				}
			}
		}
		return s
	}

	// Helper functions to construct json.Number
	numberFromFloat64 := func(f float64) json.Number {
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	}
	numberFromFloat32 := func(f float32) json.Number {
		// Use bitSize 32 to retain float32 precision characteristics
		return json.Number(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	numberFromInt64 := func(i int64) json.Number { return json.Number(strconv.FormatInt(i, 10)) }
	numberFromUint64 := func(u uint64) json.Number { return json.Number(strconv.FormatUint(u, 10)) }
	numberFromAny := func(v any) json.Number {
		switch x := v.(type) {
		case int:
			return numberFromInt64(int64(x))
		case int32:
			return numberFromInt64(int64(x))
		case int64:
			return numberFromInt64(x)
		case int8:
			return numberFromInt64(int64(x))
		case int16:
			return numberFromInt64(int64(x))
		case uint:
			return numberFromUint64(uint64(x))
		case uint32:
			return numberFromUint64(uint64(x))
		case uint64:
			return numberFromUint64(x)
		case uint8:
			return numberFromUint64(uint64(x))
		case uint16:
			return numberFromUint64(uint64(x))
		case float32:
			return numberFromFloat32(x)
		case float64:
			return numberFromFloat64(x)
		case json.Number:
			return x
		default:
			// Fallback: this path should not be used for protovalidate numeric constraints.
			// Return zero as a safe default without using fmt.
			return json.Number("0")
		}
	}

	// Switch the types, and pick a JSONSchema equivalent:
	switch desc.GetType() {

	// Float32:
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE,
		descriptor.FieldDescriptorProto_TYPE_FLOAT:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_NUMBER},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_NUMBER
		}

		// protovalidate: Double/Float simple rules -> JSON Schema
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				t := getTargetSchema(jsonSchemaType)
				if dr := fieldRules.GetDouble(); dr != nil {
					if dr.Const != nil {
						t.Const = *dr.Const
					}
					// lt / lte
					switch v := dr.GetLessThan().(type) {
					case *protovalidate.DoubleRules_Lt:
						t.ExclusiveMaximum = numberFromFloat64(v.Lt)
					case *protovalidate.DoubleRules_Lte:
						t.Maximum = numberFromFloat64(v.Lte)
					}
					// gt / gte
					switch v := dr.GetGreaterThan().(type) {
					case *protovalidate.DoubleRules_Gt:
						t.ExclusiveMinimum = numberFromFloat64(v.Gt)
					case *protovalidate.DoubleRules_Gte:
						t.Minimum = numberFromFloat64(v.Gte)
					}
					// in -> enum
					if len(dr.In) > 0 {
						for _, x := range dr.In {
							t.Enum = append(t.Enum, x)
						}
					}
					// not_in -> not: { enum }
					if len(dr.NotIn) > 0 {
						ns := &jsonschema.Schema{}
						for _, x := range dr.NotIn {
							ns.Enum = append(ns.Enum, x)
						}
						t.Not = ns
					}
				} else if fr := fieldRules.GetFloat(); fr != nil {
					if fr.Const != nil {
						t.Const = *fr.Const
					}
					// lt / lte
					switch v := fr.GetLessThan().(type) {
					case *protovalidate.FloatRules_Lt:
						t.ExclusiveMaximum = numberFromFloat32(v.Lt)
					case *protovalidate.FloatRules_Lte:
						t.Maximum = numberFromFloat32(v.Lte)
					}
					// gt / gte
					switch v := fr.GetGreaterThan().(type) {
					case *protovalidate.FloatRules_Gt:
						t.ExclusiveMinimum = numberFromFloat32(v.Gt)
					case *protovalidate.FloatRules_Gte:
						t.Minimum = numberFromFloat32(v.Gte)
					}
					// in -> enum
					if len(fr.In) > 0 {
						for _, x := range fr.In {
							t.Enum = append(t.Enum, x)
						}
					}
					// not_in -> not: { enum }
					if len(fr.NotIn) > 0 {
						ns := &jsonschema.Schema{}
						for _, x := range fr.NotIn {
							ns.Enum = append(ns.Enum, x)
						}
						t.Not = ns
					}
				}
			}
		}

	// Int32:
	case descriptor.FieldDescriptorProto_TYPE_INT32,
		descriptor.FieldDescriptorProto_TYPE_UINT32,
		descriptor.FieldDescriptorProto_TYPE_FIXED32,
		descriptor.FieldDescriptorProto_TYPE_SFIXED32,
		descriptor.FieldDescriptorProto_TYPE_SINT32:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_INTEGER},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
		}

		// protovalidate: 32-bit integer family rules -> JSON Schema
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				t := getTargetSchema(jsonSchemaType)
				apply := func(lt, lte, gt, gte any, in []any, notIn []any, cnst any) {
					if cnst != nil {
						t.Const = cnst
					}
					if lt != nil {
						t.ExclusiveMaximum = numberFromAny(lt)
					}
					if lte != nil {
						t.Maximum = numberFromAny(lte)
					}
					if gt != nil {
						t.ExclusiveMinimum = numberFromAny(gt)
					}
					if gte != nil {
						t.Minimum = numberFromAny(gte)
					}
					if len(in) > 0 {
						t.Enum = append(t.Enum, in...)
					}
					if len(notIn) > 0 {
						ns := &jsonschema.Schema{Enum: notIn}
						t.Not = ns
					}
				}
				if ir := fieldRules.GetInt32(); ir != nil {
					inVals := make([]any, 0, len(ir.In))
					for _, v := range ir.In {
						inVals = append(inVals, v)
					}
					notVals := make([]any, 0, len(ir.NotIn))
					for _, v := range ir.NotIn {
						notVals = append(notVals, v)
					}
					var lt, lte, gt, gte any
					switch v := ir.GetLessThan().(type) {
					case *protovalidate.Int32Rules_Lt:
						lt = v.Lt
					case *protovalidate.Int32Rules_Lte:
						lte = v.Lte
					}
					switch v := ir.GetGreaterThan().(type) {
					case *protovalidate.Int32Rules_Gt:
						gt = v.Gt
					case *protovalidate.Int32Rules_Gte:
						gte = v.Gte
					}
					var cnst any
					if ir.Const != nil {
						cnst = *ir.Const
					}
					apply(lt, lte, gt, gte, inVals, notVals, cnst)
				} else if ir := fieldRules.GetSint32(); ir != nil {
					inVals := make([]any, 0, len(ir.In))
					for _, v := range ir.In {
						inVals = append(inVals, v)
					}
					notVals := make([]any, 0, len(ir.NotIn))
					for _, v := range ir.NotIn {
						notVals = append(notVals, v)
					}
					var lt, lte, gt, gte any
					switch v := ir.GetLessThan().(type) {
					case *protovalidate.SInt32Rules_Lt:
						lt = v.Lt
					case *protovalidate.SInt32Rules_Lte:
						lte = v.Lte
					}
					switch v := ir.GetGreaterThan().(type) {
					case *protovalidate.SInt32Rules_Gt:
						gt = v.Gt
					case *protovalidate.SInt32Rules_Gte:
						gte = v.Gte
					}
					var cnst any
					if ir.Const != nil {
						cnst = *ir.Const
					}
					apply(lt, lte, gt, gte, inVals, notVals, cnst)
				} else if ir := fieldRules.GetSfixed32(); ir != nil {
					inVals := make([]any, 0, len(ir.In))
					for _, v := range ir.In {
						inVals = append(inVals, v)
					}
					notVals := make([]any, 0, len(ir.NotIn))
					for _, v := range ir.NotIn {
						notVals = append(notVals, v)
					}
					var lt, lte, gt, gte any
					switch v := ir.GetLessThan().(type) {
					case *protovalidate.SFixed32Rules_Lt:
						lt = v.Lt
					case *protovalidate.SFixed32Rules_Lte:
						lte = v.Lte
					}
					switch v := ir.GetGreaterThan().(type) {
					case *protovalidate.SFixed32Rules_Gt:
						gt = v.Gt
					case *protovalidate.SFixed32Rules_Gte:
						gte = v.Gte
					}
					var cnst any
					if ir.Const != nil {
						cnst = *ir.Const
					}
					apply(lt, lte, gt, gte, inVals, notVals, cnst)
				} else if ir := fieldRules.GetFixed32(); ir != nil {
					inVals := make([]any, 0, len(ir.In))
					for _, v := range ir.In {
						inVals = append(inVals, v)
					}
					notVals := make([]any, 0, len(ir.NotIn))
					for _, v := range ir.NotIn {
						notVals = append(notVals, v)
					}
					var lt, lte, gt, gte any
					switch v := ir.GetLessThan().(type) {
					case *protovalidate.Fixed32Rules_Lt:
						lt = v.Lt
					case *protovalidate.Fixed32Rules_Lte:
						lte = v.Lte
					}
					switch v := ir.GetGreaterThan().(type) {
					case *protovalidate.Fixed32Rules_Gt:
						gt = v.Gt
					case *protovalidate.Fixed32Rules_Gte:
						gte = v.Gte
					}
					var cnst any
					if ir.Const != nil {
						cnst = *ir.Const
					}
					apply(lt, lte, gt, gte, inVals, notVals, cnst)
				} else if ir := fieldRules.GetUint32(); ir != nil {
					inVals := make([]any, 0, len(ir.In))
					for _, v := range ir.In {
						inVals = append(inVals, v)
					}
					notVals := make([]any, 0, len(ir.NotIn))
					for _, v := range ir.NotIn {
						notVals = append(notVals, v)
					}
					var lt, lte, gt, gte any
					switch v := ir.GetLessThan().(type) {
					case *protovalidate.UInt32Rules_Lt:
						lt = v.Lt
					case *protovalidate.UInt32Rules_Lte:
						lte = v.Lte
					}
					switch v := ir.GetGreaterThan().(type) {
					case *protovalidate.UInt32Rules_Gt:
						gt = v.Gt
					case *protovalidate.UInt32Rules_Gte:
						gte = v.Gte
					}
					var cnst any
					if ir.Const != nil {
						cnst = *ir.Const
					}
					apply(lt, lte, gt, gte, inVals, notVals, cnst)
				}
			}
		}

	// Int64:
	case descriptor.FieldDescriptorProto_TYPE_INT64,
		descriptor.FieldDescriptorProto_TYPE_UINT64,
		descriptor.FieldDescriptorProto_TYPE_FIXED64,
		descriptor.FieldDescriptorProto_TYPE_SFIXED64,
		descriptor.FieldDescriptorProto_TYPE_SINT64:

		// As integer:
		if c.Flags.DisallowBigIntsAsStrings {
			if messageFlags.AllowNullValues {
				jsonSchemaType.OneOf = []*jsonschema.Schema{
					{Type: gojsonschema.TYPE_INTEGER},
					{Type: gojsonschema.TYPE_NULL},
				}
			} else {
				jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
			}
		}

		// As string:
		if !c.Flags.DisallowBigIntsAsStrings {
			if messageFlags.AllowNullValues {
				jsonSchemaType.OneOf = []*jsonschema.Schema{
					{Type: gojsonschema.TYPE_STRING},
					{Type: gojsonschema.TYPE_NULL},
				}
			} else {
				jsonSchemaType.Type = gojsonschema.TYPE_STRING
			}
		}

		// protovalidate: 64-bit integer family rules -> JSON Schema (only when emitted as integer)
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				t := getTargetSchema(jsonSchemaType)
				if t.Type == gojsonschema.TYPE_INTEGER { // skip if encoded as string
					apply := func(lt, lte, gt, gte any, in []any, notIn []any, cnst any) {
						if cnst != nil {
							t.Const = cnst
						}
						if lt != nil {
							t.ExclusiveMaximum = numberFromAny(lt)
						}
						if lte != nil {
							t.Maximum = numberFromAny(lte)
						}
						if gt != nil {
							t.ExclusiveMinimum = numberFromAny(gt)
						}
						if gte != nil {
							t.Minimum = numberFromAny(gte)
						}
						if len(in) > 0 {
							t.Enum = append(t.Enum, in...)
						}
						if len(notIn) > 0 {
							t.Not = &jsonschema.Schema{Enum: notIn}
						}
					}
					if ir := fieldRules.GetInt64(); ir != nil {
						inVals := make([]any, 0, len(ir.In))
						for _, v := range ir.In {
							inVals = append(inVals, v)
						}
						notVals := make([]any, 0, len(ir.NotIn))
						for _, v := range ir.NotIn {
							notVals = append(notVals, v)
						}
						var lt, lte, gt, gte any
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.Int64Rules_Lt:
							lt = v.Lt
						case *protovalidate.Int64Rules_Lte:
							lte = v.Lte
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.Int64Rules_Gt:
							gt = v.Gt
						case *protovalidate.Int64Rules_Gte:
							gte = v.Gte
						}
						var cnst any
						if ir.Const != nil {
							cnst = *ir.Const
						}
						apply(lt, lte, gt, gte, inVals, notVals, cnst)
					} else if ir := fieldRules.GetUint64(); ir != nil {
						inVals := make([]any, 0, len(ir.In))
						for _, v := range ir.In {
							inVals = append(inVals, v)
						}
						notVals := make([]any, 0, len(ir.NotIn))
						for _, v := range ir.NotIn {
							notVals = append(notVals, v)
						}
						var lt, lte, gt, gte any
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.UInt64Rules_Lt:
							lt = v.Lt
						case *protovalidate.UInt64Rules_Lte:
							lte = v.Lte
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.UInt64Rules_Gt:
							gt = v.Gt
						case *protovalidate.UInt64Rules_Gte:
							gte = v.Gte
						}
						var cnst any
						if ir.Const != nil {
							cnst = *ir.Const
						}
						apply(lt, lte, gt, gte, inVals, notVals, cnst)
					} else if ir := fieldRules.GetSfixed64(); ir != nil {
						inVals := make([]any, 0, len(ir.In))
						for _, v := range ir.In {
							inVals = append(inVals, v)
						}
						notVals := make([]any, 0, len(ir.NotIn))
						for _, v := range ir.NotIn {
							notVals = append(notVals, v)
						}
						var lt, lte, gt, gte any
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.SFixed64Rules_Lt:
							lt = v.Lt
						case *protovalidate.SFixed64Rules_Lte:
							lte = v.Lte
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.SFixed64Rules_Gt:
							gt = v.Gt
						case *protovalidate.SFixed64Rules_Gte:
							gte = v.Gte
						}
						var cnst any
						if ir.Const != nil {
							cnst = *ir.Const
						}
						apply(lt, lte, gt, gte, inVals, notVals, cnst)
					} else if ir := fieldRules.GetFixed64(); ir != nil {
						inVals := make([]any, 0, len(ir.In))
						for _, v := range ir.In {
							inVals = append(inVals, v)
						}
						notVals := make([]any, 0, len(ir.NotIn))
						for _, v := range ir.NotIn {
							notVals = append(notVals, v)
						}
						var lt, lte, gt, gte any
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.Fixed64Rules_Lt:
							lt = v.Lt
						case *protovalidate.Fixed64Rules_Lte:
							lte = v.Lte
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.Fixed64Rules_Gt:
							gt = v.Gt
						case *protovalidate.Fixed64Rules_Gte:
							gte = v.Gte
						}
						var cnst any
						if ir.Const != nil {
							cnst = *ir.Const
						}
						apply(lt, lte, gt, gte, inVals, notVals, cnst)
					} else if ir := fieldRules.GetSint64(); ir != nil {
						inVals := make([]any, 0, len(ir.In))
						for _, v := range ir.In {
							inVals = append(inVals, v)
						}
						notVals := make([]any, 0, len(ir.NotIn))
						for _, v := range ir.NotIn {
							notVals = append(notVals, v)
						}
						var lt, lte, gt, gte any
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.SInt64Rules_Lt:
							lt = v.Lt
						case *protovalidate.SInt64Rules_Lte:
							lte = v.Lte
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.SInt64Rules_Gt:
							gt = v.Gt
						case *protovalidate.SInt64Rules_Gte:
							gte = v.Gte
						}
						var cnst any
						if ir.Const != nil {
							cnst = *ir.Const
						}
						apply(lt, lte, gt, gte, inVals, notVals, cnst)
					}
				}
			}
		}

	// String:
	case descriptor.FieldDescriptorProto_TYPE_STRING:
		stringDef := &jsonschema.Schema{Type: gojsonschema.TYPE_STRING}

		// Custom field options from protoc-gen-jsonschema:
		if opt := proto.GetExtension(desc.GetOptions(), protoc_gen_jsonschema.E_FieldOptions); opt != nil {
			if fieldOptions, ok := opt.(*protoc_gen_jsonschema.FieldOptions); ok {
				if minLength := uint64(fieldOptions.GetMinLength()); minLength > 0 {
					stringDef.MinLength = &minLength
				}
				if maxLength := uint64(fieldOptions.GetMaxLength()); maxLength > 0 {
					stringDef.MaxLength = &maxLength
				}
				stringDef.Pattern = fieldOptions.GetPattern()
			}
		}

		// Custom field options from protoc-gen-validate:
		if opt := proto.GetExtension(desc.GetOptions(), protoc_gen_validate.E_Rules); opt != nil {
			if fieldRules, ok := opt.(*protoc_gen_validate.FieldRules); fieldRules != nil && ok {
				if stringRules := fieldRules.GetString_(); stringRules != nil {
					stringDef.MaxLength = stringRules.MaxLen
					stringDef.MinLength = stringRules.MinLen
					stringDef.Pattern = stringRules.GetPattern()
				}
			}
		}

		// Custom field options from protovalidate:
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				if stringRules := fieldRules.GetString(); stringRules != nil {
					stringDef.MaxLength = stringRules.MaxLen
					stringDef.MinLength = stringRules.MinLen
					stringDef.Pattern = stringRules.GetPattern()

					// len (exact length in characters)
					if stringRules.Len != nil {
						l := *stringRules.Len
						stringDef.MinLength = &l
						stringDef.MaxLength = &l
					}
					// const
					if stringRules.Const != nil {
						stringDef.Const = *stringRules.Const
					}
					// in (enum)
					if len(stringRules.In) > 0 {
						for _, v := range stringRules.In {
							stringDef.Enum = append(stringDef.Enum, v)
						}
					}
					// not_in (not enum)
					if len(stringRules.NotIn) > 0 && stringDef.Not == nil {
						notSchema := &jsonschema.Schema{}
						for _, v := range stringRules.NotIn {
							notSchema.Enum = append(notSchema.Enum, v)
						}
						stringDef.Not = notSchema
					}
					// Well-known simple formats that map 1:1 to JSON Schema
					switch {
					case stringRules.GetEmail():
						stringDef.Format = "email"
					case stringRules.GetHostname():
						stringDef.Format = "hostname"
					case stringRules.GetIpv4():
						stringDef.Format = "ipv4"
					case stringRules.GetIpv6():
						stringDef.Format = "ipv6"
					case stringRules.GetUri():
						stringDef.Format = "uri"
					case stringRules.GetUriRef():
						stringDef.Format = "uri-reference"
					case stringRules.GetUuid():
						stringDef.Format = "uuid"
					}
				}
			}
		}

		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				stringDef,
			}
		} else {
			jsonSchemaType.Type = stringDef.Type
			jsonSchemaType.MinLength = stringDef.MinLength
			jsonSchemaType.MaxLength = stringDef.MaxLength
			jsonSchemaType.Pattern = stringDef.Pattern
		}

	// Bytes:
	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{
					Type:            gojsonschema.TYPE_STRING,
					Format:          "binary",
					ContentEncoding: "base64",
				},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
			jsonSchemaType.Format = "binary"
			jsonSchemaType.ContentEncoding = "base64"
		}

	// ENUM:
	case descriptor.FieldDescriptorProto_TYPE_ENUM:

		// Go through all the enums we have, see if we can match any to this field.
		fullEnumIdentifier := strings.TrimPrefix(desc.GetTypeName(), ".")
		matchedEnum, _, ok := c.lookupEnum(curPkg, fullEnumIdentifier)
		if !ok {
			return nil, fmt.Errorf("unable to resolve enum type: %s", desc.GetType().String())
		}

		// We already have a converter for standalone ENUMs, so just use that:
		enumSchema, err := c.convertEnumType(matchedEnum, messageFlags)
		if err != nil {
			switch err {
			case errIgnored:
			default:
				return nil, err
			}
		}

		jsonSchemaType = &enumSchema

		// protovalidate: enum rules -> JSON Schema (defined_only is a no-op)
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				if er := fieldRules.GetEnum(); er != nil {
					// not_in -> not: { enum: [...] }
					if len(er.NotIn) > 0 {
						ns := &jsonschema.Schema{}
						// Disallow numeric codes
						for _, v := range er.NotIn {
							ns.Enum = append(ns.Enum, v)
						}
						// Also disallow their string names (where resolvable)
						if matchedEnum != nil {
							for _, ev := range matchedEnum.Value {
								for _, dis := range er.NotIn {
									if ev.GetNumber() == dis {
										ns.Enum = append(ns.Enum, ev.GetName())
									}
								}
							}
						}

						// Apply at the enum schema level so it covers both string and integer forms
						jsonSchemaType.Not = ns
					}
				}
			}
		}

	// Bool:
	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_BOOLEAN},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_BOOLEAN
		}
		// protovalidate: support bool.const
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				if br := fieldRules.GetBool(); br != nil && br.Const != nil {
					t := getTargetSchema(jsonSchemaType)
					t.Const = *br.Const
				}
			}
		}

	// Group (object):
	case descriptor.FieldDescriptorProto_TYPE_GROUP, descriptor.FieldDescriptorProto_TYPE_MESSAGE:

		switch desc.GetTypeName() {
		// Make sure that durations match a particular string pattern (eg 3.4s):
		case ".google.protobuf.Duration":
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
			jsonSchemaType.Format = "regex"
			jsonSchemaType.Pattern = `^([0-9]+\.?[0-9]*|\.[0-9]+)s$`
		case ".google.protobuf.Timestamp":
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
			jsonSchemaType.Format = "date-time"
		case ".google.protobuf.Value", ".google.protobuf.Struct":
			jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
			jsonSchemaType.AdditionalProperties = jsonschema.TrueSchema
		default:
			jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
			if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_OPTIONAL {
				jsonSchemaType.AdditionalProperties = jsonschema.TrueSchema
			}
			if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REQUIRED {
				jsonSchemaType.AdditionalProperties = jsonschema.FalseSchema
			}
			if messageFlags.DisallowAdditionalProperties {
				jsonSchemaType.AdditionalProperties = jsonschema.FalseSchema
			}
		}

	default:
		return nil, fmt.Errorf("unrecognized field type: %s", desc.GetType().String())
	}

	// Recurse basic array:
	if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED && jsonSchemaType.Type != gojsonschema.TYPE_OBJECT {
		jsonSchemaType.Items = &jsonschema.Schema{}

		// Custom field options from protoc-gen-validate:
		if opt := proto.GetExtension(desc.GetOptions(), protoc_gen_validate.E_Rules); opt != nil {
			if fieldRules, ok := opt.(*protoc_gen_validate.FieldRules); fieldRules != nil && ok {
				if repeatedRules := fieldRules.GetRepeated(); repeatedRules != nil {
					maxItems := repeatedRules.GetMaxItems()
					jsonSchemaType.MaxItems = &maxItems
					minItems := repeatedRules.GetMinItems()
					jsonSchemaType.MinItems = &minItems
				}
			}
		}

		// protovalidate: repeated rules -> minItems/maxItems/uniqueItems
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				if rr := fieldRules.GetRepeated(); rr != nil {
					if rr.MinItems != nil {
						mi := *rr.MinItems
						jsonSchemaType.MinItems = &mi
					}
					if rr.MaxItems != nil {
						ma := *rr.MaxItems
						jsonSchemaType.MaxItems = &ma
					}
					if rr.Unique != nil {
						jsonSchemaType.UniqueItems = rr.GetUnique()
					}
				}
			}
		}

		if len(jsonSchemaType.Enum) > 0 {
			jsonSchemaType.Items.Enum = jsonSchemaType.Enum
			jsonSchemaType.Enum = nil
			jsonSchemaType.Items.OneOf = nil
		} else {
			jsonSchemaType.Items.Type = jsonSchemaType.Type
			jsonSchemaType.Items.OneOf = jsonSchemaType.OneOf
		}

		// If the element schema carried a Not constraint (e.g., enum.not_in), move it to items
		if jsonSchemaType.Not != nil {
			jsonSchemaType.Items.Not = jsonSchemaType.Not
			jsonSchemaType.Not = nil
		}

		// protovalidate: repeated.items (FieldRules) -> apply element constraints on Items
		if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
			if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
				if rr := fieldRules.GetRepeated(); rr != nil && rr.Items != nil {
					// Work on the concrete element schema (avoid null branches in OneOf)
					it := getTargetSchema(jsonSchemaType.Items)

					// Helper to add/merge not-in enums
					appendNotEnum := func(values []any) {
						if len(values) == 0 {
							return
						}
						if it.Not == nil {
							it.Not = &jsonschema.Schema{Enum: append([]any{}, values...)}
							return
						}
						it.Not.Enum = append(it.Not.Enum, values...)
					}

					// Strings
					if sr := rr.Items.GetString(); sr != nil {
						if sr.MaxLen != nil {
							it.MaxLength = sr.MaxLen
						}
						if sr.MinLen != nil {
							it.MinLength = sr.MinLen
						}
						if sr.Len != nil { // exact length
							l := *sr.Len
							it.MinLength = &l
							it.MaxLength = &l
						}
						if p := sr.GetPattern(); p != "" {
							it.Pattern = p
						}
						if sr.Const != nil {
							it.Const = *sr.Const
						}
						if len(sr.In) > 0 {
							for _, v := range sr.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(sr.NotIn) > 0 {
							vals := make([]any, 0, len(sr.NotIn))
							for _, v := range sr.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
						// Simple well-known formats
						switch {
						case sr.GetEmail():
							it.Format = "email"
						case sr.GetHostname():
							it.Format = "hostname"
						case sr.GetIpv4():
							it.Format = "ipv4"
						case sr.GetIpv6():
							it.Format = "ipv6"
						case sr.GetUri():
							it.Format = "uri"
						case sr.GetUriRef():
							it.Format = "uri-reference"
						case sr.GetUuid():
							it.Format = "uuid"
						}
					}

					// Bool
					if br := rr.Items.GetBool(); br != nil {
						if br.Const != nil {
							it.Const = *br.Const
						}
					}

					// Float / Double
					if dr := rr.Items.GetDouble(); dr != nil {
						if dr.Const != nil {
							it.Const = *dr.Const
						}
						switch v := dr.GetLessThan().(type) {
						case *protovalidate.DoubleRules_Lt:
							it.ExclusiveMaximum = numberFromFloat64(v.Lt)
						case *protovalidate.DoubleRules_Lte:
							it.Maximum = numberFromFloat64(v.Lte)
						}
						switch v := dr.GetGreaterThan().(type) {
						case *protovalidate.DoubleRules_Gt:
							it.ExclusiveMinimum = numberFromFloat64(v.Gt)
						case *protovalidate.DoubleRules_Gte:
							it.Minimum = numberFromFloat64(v.Gte)
						}
						if len(dr.In) > 0 {
							for _, v := range dr.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(dr.NotIn) > 0 {
							vals := make([]any, 0, len(dr.NotIn))
							for _, v := range dr.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					} else if fr := rr.Items.GetFloat(); fr != nil {
						if fr.Const != nil {
							it.Const = *fr.Const
						}
						switch v := fr.GetLessThan().(type) {
						case *protovalidate.FloatRules_Lt:
							it.ExclusiveMaximum = numberFromFloat32(v.Lt)
						case *protovalidate.FloatRules_Lte:
							it.Maximum = numberFromFloat32(v.Lte)
						}
						switch v := fr.GetGreaterThan().(type) {
						case *protovalidate.FloatRules_Gt:
							it.ExclusiveMinimum = numberFromFloat32(v.Gt)
						case *protovalidate.FloatRules_Gte:
							it.Minimum = numberFromFloat32(v.Gte)
						}
						if len(fr.In) > 0 {
							for _, v := range fr.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(fr.NotIn) > 0 {
							vals := make([]any, 0, len(fr.NotIn))
							for _, v := range fr.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					}

					// 32-bit integers
					if ir := rr.Items.GetInt32(); ir != nil {
						if ir.Const != nil {
							it.Const = *ir.Const
						}
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.Int32Rules_Lt:
							it.ExclusiveMaximum = numberFromInt64(int64(v.Lt))
						case *protovalidate.Int32Rules_Lte:
							it.Maximum = numberFromInt64(int64(v.Lte))
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.Int32Rules_Gt:
							it.ExclusiveMinimum = numberFromInt64(int64(v.Gt))
						case *protovalidate.Int32Rules_Gte:
							it.Minimum = numberFromInt64(int64(v.Gte))
						}
						if len(ir.In) > 0 {
							for _, v := range ir.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(ir.NotIn) > 0 {
							vals := make([]any, 0, len(ir.NotIn))
							for _, v := range ir.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					} else if ir := rr.Items.GetSint32(); ir != nil {
						if ir.Const != nil {
							it.Const = *ir.Const
						}
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.SInt32Rules_Lt:
							it.ExclusiveMaximum = numberFromInt64(int64(v.Lt))
						case *protovalidate.SInt32Rules_Lte:
							it.Maximum = numberFromInt64(int64(v.Lte))
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.SInt32Rules_Gt:
							it.ExclusiveMinimum = numberFromInt64(int64(v.Gt))
						case *protovalidate.SInt32Rules_Gte:
							it.Minimum = numberFromInt64(int64(v.Gte))
						}
						if len(ir.In) > 0 {
							for _, v := range ir.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(ir.NotIn) > 0 {
							vals := make([]any, 0, len(ir.NotIn))
							for _, v := range ir.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					} else if ir := rr.Items.GetFixed32(); ir != nil {
						if ir.Const != nil {
							it.Const = *ir.Const
						}
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.Fixed32Rules_Lt:
							it.ExclusiveMaximum = numberFromUint64(uint64(v.Lt))
						case *protovalidate.Fixed32Rules_Lte:
							it.Maximum = numberFromUint64(uint64(v.Lte))
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.Fixed32Rules_Gt:
							it.ExclusiveMinimum = numberFromUint64(uint64(v.Gt))
						case *protovalidate.Fixed32Rules_Gte:
							it.Minimum = numberFromUint64(uint64(v.Gte))
						}
						if len(ir.In) > 0 {
							for _, v := range ir.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(ir.NotIn) > 0 {
							vals := make([]any, 0, len(ir.NotIn))
							for _, v := range ir.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					} else if ir := rr.Items.GetSfixed32(); ir != nil {
						if ir.Const != nil {
							it.Const = *ir.Const
						}
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.SFixed32Rules_Lt:
							it.ExclusiveMaximum = numberFromInt64(int64(v.Lt))
						case *protovalidate.SFixed32Rules_Lte:
							it.Maximum = numberFromInt64(int64(v.Lte))
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.SFixed32Rules_Gt:
							it.ExclusiveMinimum = numberFromInt64(int64(v.Gt))
						case *protovalidate.SFixed32Rules_Gte:
							it.Minimum = numberFromInt64(int64(v.Gte))
						}
						if len(ir.In) > 0 {
							for _, v := range ir.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(ir.NotIn) > 0 {
							vals := make([]any, 0, len(ir.NotIn))
							for _, v := range ir.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					} else if ir := rr.Items.GetUint32(); ir != nil {
						if ir.Const != nil {
							it.Const = *ir.Const
						}
						switch v := ir.GetLessThan().(type) {
						case *protovalidate.UInt32Rules_Lt:
							it.ExclusiveMaximum = numberFromUint64(uint64(v.Lt))
						case *protovalidate.UInt32Rules_Lte:
							it.Maximum = numberFromUint64(uint64(v.Lte))
						}
						switch v := ir.GetGreaterThan().(type) {
						case *protovalidate.UInt32Rules_Gt:
							it.ExclusiveMinimum = numberFromUint64(uint64(v.Gt))
						case *protovalidate.UInt32Rules_Gte:
							it.Minimum = numberFromUint64(uint64(v.Gte))
						}
						if len(ir.In) > 0 {
							for _, v := range ir.In {
								it.Enum = append(it.Enum, v)
							}
						}
						if len(ir.NotIn) > 0 {
							vals := make([]any, 0, len(ir.NotIn))
							for _, v := range ir.NotIn {
								vals = append(vals, v)
							}
							appendNotEnum(vals)
						}
					}

					// 64-bit integers (apply only if items emitted as integer)
					if it.Type == gojsonschema.TYPE_INTEGER {
						if ir := rr.Items.GetInt64(); ir != nil {
							if ir.Const != nil {
								it.Const = *ir.Const
							}
							switch v := ir.GetLessThan().(type) {
							case *protovalidate.Int64Rules_Lt:
								it.ExclusiveMaximum = numberFromInt64(v.Lt)
							case *protovalidate.Int64Rules_Lte:
								it.Maximum = numberFromInt64(v.Lte)
							}
							switch v := ir.GetGreaterThan().(type) {
							case *protovalidate.Int64Rules_Gt:
								it.ExclusiveMinimum = numberFromInt64(v.Gt)
							case *protovalidate.Int64Rules_Gte:
								it.Minimum = numberFromInt64(v.Gte)
							}
							if len(ir.In) > 0 {
								for _, v := range ir.In {
									it.Enum = append(it.Enum, v)
								}
							}
							if len(ir.NotIn) > 0 {
								vals := make([]any, 0, len(ir.NotIn))
								for _, v := range ir.NotIn {
									vals = append(vals, v)
								}
								appendNotEnum(vals)
							}
						} else if ir := rr.Items.GetUint64(); ir != nil {
							if ir.Const != nil {
								it.Const = *ir.Const
							}
							switch v := ir.GetLessThan().(type) {
							case *protovalidate.UInt64Rules_Lt:
								it.ExclusiveMaximum = numberFromUint64(v.Lt)
							case *protovalidate.UInt64Rules_Lte:
								it.Maximum = numberFromUint64(v.Lte)
							}
							switch v := ir.GetGreaterThan().(type) {
							case *protovalidate.UInt64Rules_Gt:
								it.ExclusiveMinimum = numberFromUint64(v.Gt)
							case *protovalidate.UInt64Rules_Gte:
								it.Minimum = numberFromUint64(v.Gte)
							}
							if len(ir.In) > 0 {
								for _, v := range ir.In {
									it.Enum = append(it.Enum, v)
								}
							}
							if len(ir.NotIn) > 0 {
								vals := make([]any, 0, len(ir.NotIn))
								for _, v := range ir.NotIn {
									vals = append(vals, v)
								}
								appendNotEnum(vals)
							}
						} else if ir := rr.Items.GetFixed64(); ir != nil {
							if ir.Const != nil {
								it.Const = *ir.Const
							}
							switch v := ir.GetLessThan().(type) {
							case *protovalidate.Fixed64Rules_Lt:
								it.ExclusiveMaximum = numberFromUint64(v.Lt)
							case *protovalidate.Fixed64Rules_Lte:
								it.Maximum = numberFromUint64(v.Lte)
							}
							switch v := ir.GetGreaterThan().(type) {
							case *protovalidate.Fixed64Rules_Gt:
								it.ExclusiveMinimum = numberFromUint64(v.Gt)
							case *protovalidate.Fixed64Rules_Gte:
								it.Minimum = numberFromUint64(v.Gte)
							}
							if len(ir.In) > 0 {
								for _, v := range ir.In {
									it.Enum = append(it.Enum, v)
								}
							}
							if len(ir.NotIn) > 0 {
								vals := make([]any, 0, len(ir.NotIn))
								for _, v := range ir.NotIn {
									vals = append(vals, v)
								}
								appendNotEnum(vals)
							}
						} else if ir := rr.Items.GetSfixed64(); ir != nil {
							if ir.Const != nil {
								it.Const = *ir.Const
							}
							switch v := ir.GetLessThan().(type) {
							case *protovalidate.SFixed64Rules_Lt:
								it.ExclusiveMaximum = numberFromInt64(v.Lt)
							case *protovalidate.SFixed64Rules_Lte:
								it.Maximum = numberFromInt64(v.Lte)
							}
							switch v := ir.GetGreaterThan().(type) {
							case *protovalidate.SFixed64Rules_Gt:
								it.ExclusiveMinimum = numberFromInt64(v.Gt)
							case *protovalidate.SFixed64Rules_Gte:
								it.Minimum = numberFromInt64(v.Gte)
							}
							if len(ir.In) > 0 {
								for _, v := range ir.In {
									it.Enum = append(it.Enum, v)
								}
							}
							if len(ir.NotIn) > 0 {
								vals := make([]any, 0, len(ir.NotIn))
								for _, v := range ir.NotIn {
									vals = append(vals, v)
								}
								appendNotEnum(vals)
							}
						} else if ir := rr.Items.GetSint64(); ir != nil {
							if ir.Const != nil {
								it.Const = *ir.Const
							}
							switch v := ir.GetLessThan().(type) {
							case *protovalidate.SInt64Rules_Lt:
								it.ExclusiveMaximum = numberFromInt64(v.Lt)
							case *protovalidate.SInt64Rules_Lte:
								it.Maximum = numberFromInt64(v.Lte)
							}
							switch v := ir.GetGreaterThan().(type) {
							case *protovalidate.SInt64Rules_Gt:
								it.ExclusiveMinimum = numberFromInt64(v.Gt)
							case *protovalidate.SInt64Rules_Gte:
								it.Minimum = numberFromInt64(v.Gte)
							}
							if len(ir.In) > 0 {
								for _, v := range ir.In {
									it.Enum = append(it.Enum, v)
								}
							}
							if len(ir.NotIn) > 0 {
								vals := make([]any, 0, len(ir.NotIn))
								for _, v := range ir.NotIn {
									vals = append(vals, v)
								}
								appendNotEnum(vals)
							}
						}
					}

					// Enum rules on items
					if er := rr.Items.GetEnum(); er != nil {
						// not_in -> not: { enum: [...] }
						if len(er.NotIn) > 0 {
							vals := make([]any, 0, len(er.NotIn))
							for _, v := range er.NotIn {
								vals = append(vals, v)
							}
							// Also try to add string names when available
							if matchedEnum, _, ok := c.lookupEnum(curPkg, strings.TrimPrefix(desc.GetTypeName(), ".")); ok && matchedEnum != nil {
								for _, ev := range matchedEnum.Value {
									for _, dis := range er.NotIn {
										if ev.GetNumber() == dis {
											vals = append(vals, ev.GetName())
										}
									}
								}
							}
							appendNotEnum(vals)
						}
						// in -> restrict to subset by setting enum directly (only when present)
						if len(er.In) > 0 {
							it.Enum = nil
							for _, v := range er.In {
								it.Enum = append(it.Enum, v)
							}
						}
					}
				}
			}
		}

		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_ARRAY},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
			jsonSchemaType.OneOf = []*jsonschema.Schema{}
		}
		return jsonSchemaType, nil
	}

	// Recurse nested objects / arrays of objects (if necessary):
	if jsonSchemaType.Type == gojsonschema.TYPE_OBJECT {

		recordType, pkgName, ok := c.lookupType(curPkg, desc.GetTypeName())
		if !ok {
			return nil, fmt.Errorf("no such message type named %s", desc.GetTypeName())
		}

		// Recurse the recordType:
		recursedJSONSchemaType, err := c.recursiveConvertMessageType(curPkg, recordType, pkgName, duplicatedMessages, false)
		if err != nil {
			return nil, err
		}

		// Maps, arrays, and objects are structured in different ways:
		switch {

		// Maps:
		case recordType.Options.GetMapEntry():
			c.logger.
				WithField("field_name", recordType.GetName()).
				WithField("msgDesc_name", *msgDesc.Name).
				Tracef("Is a map")

			if recursedJSONSchemaType.Properties == nil {
				return nil, fmt.Errorf("Unable to find properties of MAP type")
			}

			// Make sure we have a "value":
			value, valuePresent := recursedJSONSchemaType.Properties.Get("value")
			if !valuePresent {
				return nil, fmt.Errorf("Unable to find 'value' property of MAP type")
			}

			jsonSchemaType.AdditionalProperties = value

			// protovalidate: map min_pairs/max_pairs -> minProperties/maxProperties
			if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
				if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
					if mr := fieldRules.GetMap(); mr != nil {
						if mr.MinPairs != nil {
							mp := *mr.MinPairs
							jsonSchemaType.MinProperties = &mp
						}
						if mr.MaxPairs != nil {
							mp := *mr.MaxPairs
							jsonSchemaType.MaxProperties = &mp
						}
					}
				}
			}

		// Arrays:
		case desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED:
			jsonSchemaType.Items = recursedJSONSchemaType
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY

			// protovalidate: repeated rules -> minItems/maxItems/uniqueItems for repeated messages
			if opt := proto.GetExtension(desc.GetOptions(), protovalidate.E_Field); opt != nil {
				if fieldRules, ok := opt.(*protovalidate.FieldRules); fieldRules != nil && ok {
					if rr := fieldRules.GetRepeated(); rr != nil {
						if rr.MinItems != nil {
							mi := *rr.MinItems
							jsonSchemaType.MinItems = &mi
						}
						if rr.MaxItems != nil {
							ma := *rr.MaxItems
							jsonSchemaType.MaxItems = &ma
						}
						if rr.Unique != nil {
							jsonSchemaType.UniqueItems = rr.GetUnique()
						}
					}
				}
			}

			// Build up the list of required fields:
			if messageFlags.AllFieldsRequired && len(recursedJSONSchemaType.OneOf) == 0 && recursedJSONSchemaType.Properties != nil {
				for pair := recursedJSONSchemaType.Properties.Oldest(); pair != nil; pair = pair.Next() {
					jsonSchemaType.Items.Required = append(jsonSchemaType.Items.Required, pair.Key)
				}
			}
			jsonSchemaType.Items.Required = dedupe(jsonSchemaType.Items.Required)

		// Not maps, not arrays:
		default:

			// If we've got optional types then just take those:
			if recursedJSONSchemaType.OneOf != nil {
				return recursedJSONSchemaType, nil
			}

			// If we're not an object then set the type from whatever we recursed:
			if recursedJSONSchemaType.Type != gojsonschema.TYPE_OBJECT {
				jsonSchemaType.Type = recursedJSONSchemaType.Type
			}

			// Assume the attrbutes of the recursed value:
			jsonSchemaType.Properties = recursedJSONSchemaType.Properties
			jsonSchemaType.Ref = recursedJSONSchemaType.Ref
			jsonSchemaType.Required = recursedJSONSchemaType.Required

			// Build up the list of required fields:
			if messageFlags.AllFieldsRequired && len(recursedJSONSchemaType.OneOf) == 0 && recursedJSONSchemaType.Properties != nil {
				for pair := recursedJSONSchemaType.Properties.Oldest(); pair != nil; pair = pair.Next() {
					jsonSchemaType.Required = append(jsonSchemaType.Required, pair.Key)
				}
			}
		}

		// Optionally allow NULL values:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_NULL},
				{Type: jsonSchemaType.Type, Items: jsonSchemaType.Items},
			}
			jsonSchemaType.Type = ""
			jsonSchemaType.Items = nil
		}
	}

	jsonSchemaType.Required = dedupe(jsonSchemaType.Required)

	return jsonSchemaType, nil
}

// Converts a proto "MESSAGE" into a JSON-Schema:
func (c *Converter) convertMessageType(curPkg *ProtoPackage, msgDesc *descriptor.DescriptorProto) (*jsonschema.Schema, error) {

	// Get a list of any nested messages in our schema:
	duplicatedMessages, err := c.findNestedMessages(curPkg, msgDesc)
	if err != nil {
		return nil, err
	}

	// Build up a list of JSONSchema type definitions for every message:
	definitions := jsonschema.Definitions{}
	for refmsgDesc, nameWithPackage := range duplicatedMessages {
		var typeName string
		if c.Flags.TypeNamesWithNoPackage {
			typeName = refmsgDesc.GetName()
		} else {
			typeName = nameWithPackage
		}
		refType, err := c.recursiveConvertMessageType(curPkg, refmsgDesc, "", duplicatedMessages, true)
		if err != nil {
			return nil, err
		}

		// Add the schema to our definitions:
		definitions[typeName] = refType
	}

	// Put together a JSON schema with our discovered definitions, and a $ref for the root type:
	newJSONSchema := &jsonschema.Schema{
		Ref:         fmt.Sprintf("%s%s", c.refPrefix, msgDesc.GetName()),
		Definitions: definitions,
	}

	return newJSONSchema, nil
}

// findNestedMessages takes a message, and returns a map mapping pointers to messages nested within it:
// these messages become definitions which can be referenced (instead of repeating them every time they're used)
func (c *Converter) findNestedMessages(curPkg *ProtoPackage, msgDesc *descriptor.DescriptorProto) (map[*descriptor.DescriptorProto]string, error) {

	// Get a list of all nested messages, and how often they occur:
	nestedMessages := make(map[*descriptor.DescriptorProto]string)
	if err := c.recursiveFindNestedMessages(curPkg, msgDesc, msgDesc.GetName(), nestedMessages); err != nil {
		return nil, err
	}

	// Now filter them:
	result := make(map[*descriptor.DescriptorProto]string)
	for message, messageName := range nestedMessages {
		if !message.GetOptions().GetMapEntry() && !strings.HasPrefix(messageName, ".google.protobuf.") {
			result[message] = strings.TrimLeft(messageName, ".")
		}
	}

	return result, nil
}

func (c *Converter) recursiveFindNestedMessages(curPkg *ProtoPackage, msgDesc *descriptor.DescriptorProto, typeName string, nestedMessages map[*descriptor.DescriptorProto]string) error {
	if _, present := nestedMessages[msgDesc]; present {
		return nil
	}
	nestedMessages[msgDesc] = typeName

	for _, desc := range msgDesc.GetField() {
		descType := desc.GetType()
		if descType != descriptor.FieldDescriptorProto_TYPE_MESSAGE && descType != descriptor.FieldDescriptorProto_TYPE_GROUP {
			// no nested messages
			continue
		}

		typeName := desc.GetTypeName()
		recordType, _, ok := c.lookupType(curPkg, typeName)
		if !ok {
			return fmt.Errorf("no such message type named %s", typeName)
		}
		if err := c.recursiveFindNestedMessages(curPkg, recordType, typeName, nestedMessages); err != nil {
			return err
		}
	}

	return nil
}

func (c *Converter) recursiveConvertMessageType(curPkg *ProtoPackage, msgDesc *descriptor.DescriptorProto, pkgName string, duplicatedMessages map[*descriptor.DescriptorProto]string, ignoreDuplicatedMessages bool) (*jsonschema.Schema, error) {

	// Prepare a new jsonschema:
	jsonSchemaType := new(jsonschema.Schema)

	// Set some per-message flags from config and options:
	messageFlags := c.Flags

	// Custom message options from protoc-gen-jsonschema:
	if opt := proto.GetExtension(msgDesc.GetOptions(), protoc_gen_jsonschema.E_MessageOptions); opt != nil {
		if messageOptions, ok := opt.(*protoc_gen_jsonschema.MessageOptions); ok {

			// AllFieldsRequired:
			if messageOptions.GetAllFieldsRequired() {
				messageFlags.AllFieldsRequired = true
			}

			// AllowNullValues:
			if messageOptions.GetAllowNullValues() {
				messageFlags.AllowNullValues = true
			}

			// DisallowAdditionalProperties:
			if messageOptions.GetDisallowAdditionalProperties() {
				messageFlags.DisallowAdditionalProperties = true
			}

			// ENUMs as constants:
			if messageOptions.GetEnumsAsConstants() {
				messageFlags.EnumsAsConstants = true
			}
		}
	}

	// Generate a description from src comments (if available)
	if src := c.sourceInfo.GetMessage(msgDesc); src != nil {
		jsonSchemaType.Title, jsonSchemaType.Description = c.formatTitleAndDescription(strPtr(msgDesc.GetName()), src)
	}

	// Handle google's well-known types:
	if msgDesc.Name != nil && wellKnownTypes[*msgDesc.Name] && pkgName == ".google.protobuf" {
		switch *msgDesc.Name {
		case "DoubleValue", "FloatValue":
			jsonSchemaType.Type = gojsonschema.TYPE_NUMBER
		case "Int32Value", "UInt32Value":
			jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
		case "Int64Value", "UInt64Value":
			// BigInt as ints
			if messageFlags.DisallowBigIntsAsStrings {
				jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
			} else {

				// BigInt as strings
				jsonSchemaType.Type = gojsonschema.TYPE_STRING
			}

		case "BoolValue":
			jsonSchemaType.Type = gojsonschema.TYPE_BOOLEAN
		case "BytesValue", "StringValue":
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
		case "Value":
			jsonSchemaType.OneOf = []*jsonschema.Schema{
				{Type: gojsonschema.TYPE_ARRAY},
				{Type: gojsonschema.TYPE_BOOLEAN},
				{Type: gojsonschema.TYPE_NUMBER},
				{Type: gojsonschema.TYPE_OBJECT},
				{Type: gojsonschema.TYPE_STRING},
			}
			// jsonSchemaType.AdditionalProperties = jsonschema.TrueSchema
		case "Duration":
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
		case "Struct":
			jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
			// jsonSchemaType.AdditionalProperties = jsonschema.TrueSchema
		case "ListValue":
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
		}

		// If we're allowing nulls then prepare a OneOf:
		if messageFlags.AllowNullValues {
			jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Schema{Type: gojsonschema.TYPE_NULL}, &jsonschema.Schema{Type: jsonSchemaType.Type})
			// and clear the Type that was previously set.
			jsonSchemaType.Type = ""
			return jsonSchemaType, nil
		}

		// Otherwise just return this simple type:
		return jsonSchemaType, nil
	}

	// Set defaults:
	jsonSchemaType.Properties = orderedmap.New[string, *jsonschema.Schema]()

	// Look up references:
	if nameWithPackage, ok := duplicatedMessages[msgDesc]; ok && !ignoreDuplicatedMessages {
		var typeName string
		if c.Flags.TypeNamesWithNoPackage {
			typeName = msgDesc.GetName()
		} else {
			typeName = nameWithPackage
		}
		return &jsonschema.Schema{
			Ref: fmt.Sprintf("%s%s", c.refPrefix, typeName),
		}, nil
	}

	// Optionally allow NULL values:
	if messageFlags.AllowNullValues {
		jsonSchemaType.OneOf = []*jsonschema.Schema{
			{Type: gojsonschema.TYPE_NULL},
			{Type: gojsonschema.TYPE_OBJECT},
		}
	} else {
		jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
	}

	// disallowAdditionalProperties will prevent validation where extra fields are found (outside of the schema):
	if messageFlags.DisallowAdditionalProperties {
		jsonSchemaType.AdditionalProperties = jsonschema.FalseSchema
	} else {
		jsonSchemaType.AdditionalProperties = jsonschema.TrueSchema
	}

	c.logger.WithField("message_str", msgDesc.String()).Trace("Converting message")
	for _, fieldDesc := range msgDesc.GetField() {

		// Custom field options from protoc-gen-jsonschema:
		if opt := proto.GetExtension(fieldDesc.GetOptions(), protoc_gen_jsonschema.E_FieldOptions); opt != nil {
			if fieldOptions, ok := opt.(*protoc_gen_jsonschema.FieldOptions); ok {

				// "Ignored" fields are simply skipped:
				if fieldOptions.GetIgnore() {
					c.logger.WithField("field_name", fieldDesc.GetName()).WithField("message_name", msgDesc.GetName()).Debug("Skipping ignored field")
					continue
				}

				// "Required" fields are added to the list of required attributes in our schema:
				if fieldOptions.GetRequired() {
					c.logger.WithField("field_name", fieldDesc.GetName()).WithField("message_name", msgDesc.GetName()).Debug("Marking required field")
					if c.Flags.UseJSONFieldnamesOnly {
						jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetJsonName())
					} else {
						jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetName())
					}
				}
			}
		}

		// Convert the field into a JSONSchema type:
		recursedJSONSchemaType, err := c.convertField(curPkg, fieldDesc, msgDesc, duplicatedMessages, messageFlags)
		if err != nil {
			c.logger.WithError(err).WithField("field_name", fieldDesc.GetName()).WithField("message_name", msgDesc.GetName()).Error("Failed to convert field")
			return nil, err
		}
		c.logger.WithField("field_name", fieldDesc.GetName()).WithField("type", recursedJSONSchemaType.Type).Trace("Converted field")

		// If this field is part of a OneOf declaration then build that here:
		if c.Flags.EnforceOneOf && fieldDesc.OneofIndex != nil && !fieldDesc.GetProto3Optional() {
			for {
				if *fieldDesc.OneofIndex < int32(len(jsonSchemaType.AllOf)) {
					break
				}
				var notAnyOf = &jsonschema.Schema{Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{}}}
				jsonSchemaType.AllOf = append(jsonSchemaType.AllOf, &jsonschema.Schema{OneOf: []*jsonschema.Schema{notAnyOf}})
			}
			if c.Flags.UseJSONFieldnamesOnly {
				jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf = append(jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf, &jsonschema.Schema{Required: []string{fieldDesc.GetJsonName()}})
				jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf[0].Not.AnyOf = append(jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf[0].Not.AnyOf, &jsonschema.Schema{Required: []string{fieldDesc.GetJsonName()}})
			} else {
				jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf = append(jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf, &jsonschema.Schema{Required: []string{fieldDesc.GetName()}})
				jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf[0].Not.AnyOf = append(jsonSchemaType.AllOf[*fieldDesc.OneofIndex].OneOf[0].Not.AnyOf, &jsonschema.Schema{Required: []string{fieldDesc.GetName()}})
			}
		}

		// Figure out which field names we want to use:
		switch {
		case c.Flags.UseJSONFieldnamesOnly:
			jsonSchemaType.Properties.Set(fieldDesc.GetJsonName(), recursedJSONSchemaType)
		case c.Flags.UseProtoAndJSONFieldNames:
			jsonSchemaType.Properties.Set(fieldDesc.GetName(), recursedJSONSchemaType)
			jsonSchemaType.Properties.Set(fieldDesc.GetJsonName(), recursedJSONSchemaType)
		default:
			jsonSchemaType.Properties.Set(fieldDesc.GetName(), recursedJSONSchemaType)
		}

		// Enforce all_fields_required:
		if messageFlags.AllFieldsRequired {
			if fieldDesc.OneofIndex == nil && !fieldDesc.GetProto3Optional() {
				if c.Flags.UseJSONFieldnamesOnly {
					jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetJsonName())
				} else {
					jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetName())
				}
			}
		}

		// Look for required fields by the proto2 "required" flag:
		if fieldDesc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REQUIRED && fieldDesc.OneofIndex == nil {
			if c.Flags.UseJSONFieldnamesOnly {
				jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetJsonName())
			} else {
				jsonSchemaType.Required = append(jsonSchemaType.Required, fieldDesc.GetName())
			}
		}
	}

	// Remove empty properties to keep the final output as clean as possible:
	if jsonSchemaType.Properties.Len() == 0 {
		jsonSchemaType.Properties = nil
	}

	// Dedupe required fields:
	jsonSchemaType.Required = dedupe(jsonSchemaType.Required)

	return jsonSchemaType, nil
}

func dedupe(inputStrings []string) []string {
	appended := make(map[string]bool)
	outputStrings := []string{}

	for _, inputString := range inputStrings {
		if !appended[inputString] {
			outputStrings = append(outputStrings, inputString)
			appended[inputString] = true
		}
	}
	return outputStrings
}
