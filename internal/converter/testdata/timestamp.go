package testdata

const Timestamp = `{
    "$schema": "http://json-schema.org/draft-04/schema#",
    "$ref": "#/$defs/Timestamp",
    "$defs": {
        "Timestamp": {
            "properties": {
                "timestamp": {
                    "type": "string",
                    "format": "date-time"
                }
            },
            "additionalProperties": true,
            "type": "object",
            "title": "Timestamp"
        }
    }
}`

const TimestampFail = `{"timestamp": "twelve oclock"}`

const TimestampPass = `{"timestamp": "1970-01-01T00:00:00Z"}`
