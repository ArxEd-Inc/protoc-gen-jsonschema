package testdata

const SelfReference = `{
    "$schema": "http://json-schema.org/draft-04/schema#",
    "$ref": "#/$defs/Foo",
    "$defs": {
        "Foo": {
            "properties": {
                "name": {
                    "type": "string"
                },
                "bar": {
                    "items": {
                        "$ref": "#/$defs/Foo"
                    },
                    "type": "array"
                }
            },
            "additionalProperties": true,
            "type": "object",
            "title": "Foo"
        }
    }
}`

const SelfReferenceFail = `{
	"bar": [
		{
			"name": false
		}
	]
}`

const SelfReferencePass = `{
	"bar": [
		{
			"name": "referenced-bar",
			"bar": [
				{
					"name": "barception"
				}
			]
		}
	]
}`
