// Package dynamodb reads parameters from an Amazon DynamoDB table, with the
// cache and the transforms of package parameters.
//
// The table follows Python's schema, whose attribute names can be changed
// with WithKeyAttribute, WithSortAttribute and WithValueAttribute:
//
//   - a single value is the item whose partition key "id" is its name, and
//     its "value" attribute: a table with only a partition key;
//   - the values under a path are the items whose partition key is the path,
//     keyed by their sort key "sk": a table with a sort key.
//
// Create a Provider once and share it:
//
//	var table = dynamodb.FromConfig(cfg, "ParameterTable") // or dynamodb.New(client, ...)
//
//	value, err := table.Get(ctx, "my-param")
//	values, err := table.GetMultiple(ctx, "my-hash-key")
//
// Get calls GetItem, GetMultiple calls Query, every page;
// WithConsistentRead makes them strongly consistent. There is no shortcut,
// since a table must be named.
//
// # Differences from Python
//
//   - Python's boto3 resource returns the value attribute as a Python value
//     of any type. Here a string is returned as it is, a binary as its bytes,
//     a number as its text, and any other type (map, list, set, boolean,
//     null) as its JSON encoding, which parameters.TransformJSON decodes into
//     a Go type.
//   - A local DynamoDB is reached through the BaseEndpoint of the AWS
//     configuration, Python's endpoint_url.
package dynamodb
