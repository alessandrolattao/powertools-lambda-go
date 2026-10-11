package datamasking_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// jsonText renders a result as JSON with sorted keys.
func jsonText(v any) string {
	out, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return string(out)
}

func ExampleDataMasking_Erase() {
	masker := datamasking.New()
	body := []byte(`{
		"name": "John Doe",
		"email": "johndoe@example.com",
		"address": {"street": "123 Main St", "zip": "12345"},
		"company_address": {"street": "456 ACME Ave", "zip": "12345"}
	}`)

	erased, err := masker.Erase(body, datamasking.WithFields("email", "address.street", "company_address"))
	if err != nil {
		panic(err)
	}
	fmt.Println(jsonText(erased))
	// Output: {"address":{"street":"*****","zip":"12345"},"company_address":"*****","email":"*****","name":"John Doe"}
}

func ExampleDataMasking_Erase_wholeData() {
	masker := datamasking.New()

	erased, err := masker.Erase([]string{"name", "phone"})
	if err != nil {
		panic(err)
	}
	fmt.Println(jsonText(erased))
	// Output: ["*****","*****"]
}

func ExampleDataMasking_Erase_customMasks() {
	masker := datamasking.New()
	body := `{"email": "johndoe@example.com", "address": {"street": "123 Main St", "zip": "12345"}}`

	for _, mask := range []datamasking.EraseOption{
		datamasking.WithDynamicMask(),
		datamasking.WithCustomMask("XX"),
		datamasking.WithRegex(`(.)(.*)(@.*)`, `\1****\3`),
	} {
		erased, err := masker.Erase(body, datamasking.WithFields("address.street", "address.zip", "email"), mask)
		if err != nil {
			panic(err)
		}
		fmt.Println(jsonText(erased))
	}
	// Output:
	// {"address":{"street":"*** **** **","zip":"*****"},"email":"***************.***"}
	// {"address":{"street":"XX","zip":"XX"},"email":"XX"}
	// {"address":{"street":"123 Main St","zip":"12345"},"email":"j****@example.com"}
}

func ExampleWithMaskingRules() {
	masker := datamasking.New()
	body := []byte(`{
		"age": 30,
		"email": "janedoe@example.com",
		"address": {"zip": "12345"},
		"other_address": [{"postcode": 11345}, {"postcode": 67890}]
	}`)

	erased, err := masker.Erase(body, datamasking.WithMaskingRules(
		datamasking.Rule{Path: "email", RegexPattern: `(.)(.*)(@.*)`, MaskFormat: `\1****\3`},
		datamasking.Rule{Path: "age", DynamicMask: true},
		datamasking.Rule{Path: "address.zip", CustomMask: "xxx"},
		datamasking.Rule{Path: "$.other_address[?(@.postcode > 12000)]", CustomMask: "Masked"},
	))
	if err != nil {
		panic(err)
	}
	fmt.Println(jsonText(erased))
	// Output: {"address":{"zip":"xxx"},"age":"**","email":"j****@example.com","other_address":[{"postcode":11345},"Masked"]}
}

func ExampleDataMasking_EraseAs() {
	type customer struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	masker := datamasking.New()

	c, err := masker.EraseAs[customer](customer{Name: "Pat", Email: "pat@example.com"}, datamasking.WithFields("email"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%+v\n", c)

	raw, err := masker.EraseAs[jsontext.Value](`{"card_number": "1111 2222 3333 4444"}`, datamasking.WithFields("card_number"))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))
	// Output:
	// {Name:Pat Email:*****}
	// {"card_number":"*****"}
}

// reverseProvider stands in for a real one, such as datamasking/kms.
type reverseProvider struct{}

func (reverseProvider) Encrypt(_ context.Context, plaintext []byte, _ datamasking.Request) (string, error) {
	out := []byte(string(plaintext))
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

func (p reverseProvider) Decrypt(ctx context.Context, ciphertext string, req datamasking.Request) ([]byte, error) {
	plaintext, err := p.Encrypt(ctx, []byte(ciphertext), req)
	return []byte(plaintext), err
}

func ExampleDataMasking_Encrypt() {
	masker := datamasking.New(datamasking.WithProvider(reverseProvider{}))
	ctx := context.Background()

	ciphertext, err := masker.Encrypt(ctx, map[string]any{"card_number": "1111"})
	if err != nil {
		panic(err)
	}
	type card struct {
		Number string `json:"card_number"`
	}
	c, err := masker.DecryptAs[card](ctx, ciphertext)
	if err != nil {
		panic(err)
	}
	fmt.Println(c.Number)
	// Output: 1111
}
