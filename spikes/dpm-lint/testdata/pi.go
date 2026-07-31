package main

import "github.com/stripe/stripe-go/v79"

func main() {
	// payment_method_types in a comment must NOT match
	params := &stripe.PaymentIntentParams{
		Amount:             stripe.Int64(1099),
		Currency:           stripe.String("eur"),
		PaymentMethodTypes: stripe.StringSlice([]string{"card", "ideal"}),
	}
	_ = params
}
