package main

import (
	"fmt"
	"testing"
)

// TestPacks asserts each authored pack's fixture corpus produces exactly the
// expected finding count per file — positives fire, negatives stay silent.
func TestPacks(t *testing.T) {
	cases := map[string]struct {
		rule   Rule
		expect map[string]int
	}{
		"tax-percent": {rule: taxPercentRule, expect: map[string]int{
			"negative.js":  0,
			"negative.py":  0,
			"negative.rb":  0,
			"positive.js":  1,
			"positive.php": 2,
			"positive.py":  1,
			"positive.rb":  2,
		}},
		"collection-method": {rule: collectionMethodRule, expect: map[string]int{
			"negative.js":  0,
			"negative.php": 0,
			"negative.py":  0,
			"negative.rb":  0,
			"positive.js":  2,
			"positive.php": 1,
			"positive.py":  2,
			"positive.rb":  4,
		}},
		"prorate": {rule: prorateRule, expect: map[string]int{
			"negative.js":  0,
			"negative.py":  0,
			"negative.rb":  0,
			"positive.js":  2,
			"positive.php": 1,
			"positive.py":  1,
			"positive.rb":  2,
		}},
		"source-types": {rule: sourceTypesRule, expect: map[string]int{
			"negative_comment.py":         0,
			"negative_source_resource.rb": 0,
			"negative_unbound_object.js":  0,
			"positive_confirm.py":         1,
			"positive_create.php":         1,
			"positive_create.rb":          1,
			"positive_update.js":          1,
		}},
		"ewcs": {rule: ewcsRule, expect: map[string]int{
			"server.js":        1,
			"subscribe.rb":     1,
			"checkout_form.js": 0,
			"webhook.rb":       0,
			"negative.js":      0,
		}},
		"flex": {rule: flexRule, expect: map[string]int{
			"positive.rb": 1,
			"capture.js":  1,
			"negative.py": 0,
		}},
	}
	for topic, c := range cases {
		t.Run(topic, func(t *testing.T) {
			dir := fmt.Sprintf("testdata-packs/%s", topic)
			findings, _, _, err := scan(dir, c.rule)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, f := range findings {
				got[f.File[len(dir)+1:]]++
			}
			for file, want := range c.expect {
				if got[file] != want {
					t.Errorf("%s/%s: got %d findings, want %d", topic, file, got[file], want)
				}
			}
			for file, n := range got {
				if _, known := c.expect[file]; !known {
					t.Errorf("%s/%s: %d unexpected findings", topic, file, n)
				}
			}
		})
	}
}

// TestPackSignals pins the pack-declared signal layer: the EWCS readiness
// fixtures must show all four expected events missing, from-state frontend
// tokens present, and both package floors violated — while the DPM fixtures
// keep their original three-events-present / Card-Element-warning behavior.
func TestPackSignals(t *testing.T) {
	h, fw, mc := scanSignals("testdata-packs/ewcs", &ewcsSignals)
	if h.AllPresent {
		t.Error("ewcs: expected missing checkout.session.* handlers")
	}
	for _, e := range h.Events {
		if e.Present {
			t.Errorf("ewcs: event %s unexpectedly present", e.Event)
		}
	}
	if len(fw) == 0 {
		t.Error("ewcs: expected frontend warnings in checkout_form.js")
	}
	bad := 0
	for _, m := range mc {
		if !m.OK {
			bad++
		}
	}
	if bad != 2 {
		t.Errorf("ewcs: expected 2 failed manifest floors, got %d (%v)", bad, mc)
	}

	h2, fw2, _ := scanSignals("testdata", &dpmSignals)
	if !h2.AllPresent {
		t.Errorf("dpm: expected all three delayed-notification events present: %+v", h2.Events)
	}
	if len(fw2) == 0 {
		t.Error("dpm: expected the Card Element warning from frontend_card.js")
	}
}
