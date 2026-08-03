package main

// fix_test.go — the version-forked companion behavior end to end: insert
// branch (below-cutoff / unknown accounts), omit branch (at/after cutoff),
// resource gating, idempotence, and whitespace-clean removals.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func copyFixtures(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		src, err := os.ReadFile(filepath.Join("testdata", n))
		if err != nil {
			t.Fatalf("read fixture %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), src, 0o644); err != nil {
			t.Fatalf("copy fixture %s: %v", n, err)
		}
	}
	return dir
}

func mustRead(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

var wsOnlyLine = regexp.MustCompile(`(?m)^[ \t]+$`)

func TestCompanionInsertLanguages(t *testing.T) {
	dir := copyFixtures(t, "pi.py", "var_indirect.py", "pi.rb", "pi.js", "pi.php", "pi.go", "Pi.cs", "Pi.java", "sub.rb")
	dec := &companionDecision{insert: true, reason: "test: below cutoff"}
	rep, err := fixRun(dir, dpmRule, true, false, dec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatalf("expected every reparse clean, got %+v", rep.Files)
	}
	if rep.Companion == nil || rep.Companion.Mode != "insert" || rep.Companion.Inserts == 0 {
		t.Fatalf("expected companion insert with >0 inserts, got %+v", rep.Companion)
	}

	want := map[string]string{
		"pi.py":           `automatic_payment_methods={"enabled": True}`,
		"var_indirect.py": `"automatic_payment_methods": {"enabled": True}`,
		"pi.rb":           "automatic_payment_methods: {enabled: true}",
		"pi.js":           "automatic_payment_methods: {enabled: true}",
		"pi.php":          "'automatic_payment_methods' => ['enabled' => true]",
		"pi.go":           "AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)}",
		"Pi.cs":           "AutomaticPaymentMethods = new PaymentIntentAutomaticPaymentMethodsOptions { Enabled = true }",
		"Pi.java":         ".setAutomaticPaymentMethods(PaymentIntentCreateParams.AutomaticPaymentMethods.builder().setEnabled(true).build())",
	}
	for name, ins := range want {
		if out := mustRead(t, dir, name); !strings.Contains(out, ins) {
			t.Errorf("%s must gain the companion %q; got:\n%s", name, ins, out)
		}
	}

	// Nested payment_settings.payment_method_types (subscriptions) is removed
	// but must NOT gain the companion — the parameter does not exist there.
	if sub := mustRead(t, dir, "sub.rb"); strings.Contains(sub, "automatic_payment_methods") {
		t.Errorf("sub.rb must not gain the companion:\n%s", sub)
	}

	// The migration is complete: a rescan finds nothing.
	findings, _, _, err := scan(dir, dpmRule)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("rescan after apply must be clean, got %d findings", len(findings))
	}
}

func TestCompanionOmittedIsPlainRemoval(t *testing.T) {
	dir := copyFixtures(t, "pi.py", "pi.php")
	dec := &companionDecision{insert: false, reason: "test: at/after cutoff"}
	rep, err := fixRun(dir, dpmRule, true, false, dec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected all reparse clean")
	}
	if rep.Companion == nil || rep.Companion.Mode != "omit" || rep.Companion.Inserts != 0 {
		t.Fatalf("expected companion omit with 0 inserts, got %+v", rep.Companion)
	}
	for _, name := range []string{"pi.py", "pi.php"} {
		out := mustRead(t, dir, name)
		if strings.Contains(out, "automatic_payment_methods") {
			t.Errorf("%s: omit branch must not insert:\n%s", name, out)
		}
		if wsOnlyLine.MatchString(out) {
			t.Errorf("%s: removal must not leave whitespace-only lines:\n%s", name, out)
		}
	}
}

func TestCompanionResourceGate(t *testing.T) {
	// Checkout Sessions have no automatic_payment_methods parameter: the
	// removal must stay a removal even on the insert branch.
	dir := t.TempDir()
	src := `const stripe = require('stripe')('sk_test_x');
await stripe.checkout.sessions.create({
  mode: 'payment',
  payment_method_types: ['card', 'ideal'],
  success_url: 'https://example.com',
});
`
	if err := os.WriteFile(filepath.Join(dir, "sessions.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	out := mustRead(t, dir, "sessions.js")
	if strings.Contains(out, "payment_method_types") {
		t.Errorf("param must be removed:\n%s", out)
	}
	if strings.Contains(out, "automatic_payment_methods") {
		t.Errorf("checkout sessions must not gain the companion:\n%s", out)
	}
}

func TestCompanionIdempotent(t *testing.T) {
	// A call that already sets automatic_payment_methods must not gain a
	// second one; the stale payment_method_types is still removed.
	dir := t.TempDir()
	src := `const stripe = require('stripe')('sk_test_x');
await stripe.paymentIntents.create({
  amount: 1099,
  currency: 'eur',
  payment_method_types: ['card', 'ideal'],
  automatic_payment_methods: {enabled: true},
});
`
	if err := os.WriteFile(filepath.Join(dir, "pi.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	out := mustRead(t, dir, "pi.js")
	if strings.Contains(out, "payment_method_types") {
		t.Errorf("param must be removed:\n%s", out)
	}
	if n := strings.Count(out, "automatic_payment_methods"); n != 1 {
		t.Errorf("expected exactly one companion, got %d:\n%s", n, out)
	}
}

func TestCompanionJavaSingleInsertPerBuilder(t *testing.T) {
	// Two addPaymentMethodType statements on ONE builder must yield exactly
	// one setAutomaticPaymentMethods, or the call would set it twice.
	dir := t.TempDir()
	src := `import com.stripe.param.PaymentIntentCreateParams;

class Demo {
  void go() {
    PaymentIntentCreateParams.Builder paramsBuilder = PaymentIntentCreateParams.builder();
    paramsBuilder.setAmount(1099L);
    paramsBuilder.addPaymentMethodType("card");
    paramsBuilder.addPaymentMethodType("link");
    paramsBuilder.build();
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "Server.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	out := mustRead(t, dir, "Server.java")
	if strings.Contains(out, "addPaymentMethodType") {
		t.Errorf("both statements must be handled:\n%s", out)
	}
	if n := strings.Count(out, "setAutomaticPaymentMethods"); n != 1 {
		t.Errorf("expected exactly one companion per builder, got %d:\n%s", n, out)
	}
	if wsOnlyLine.MatchString(out) {
		t.Errorf("statement removal must not leave whitespace-only lines:\n%s", out)
	}
}

func TestCompanionDistinctBuildersEachGetInsert(t *testing.T) {
	// Review finding: the dedupe key must identify a builder INSTANCE. Two
	// chains in one file, and same-named builders in different methods, must
	// each get their own companion.
	dir := t.TempDir()
	chains := `import com.stripe.param.PaymentIntentCreateParams;
import com.stripe.param.SetupIntentCreateParams;

class Demo {
  void a() {
    PaymentIntentCreateParams p = PaymentIntentCreateParams.builder().setAmount(1099L).addPaymentMethodType("card").build();
  }
  void b() {
    SetupIntentCreateParams s = SetupIntentCreateParams.builder().addPaymentMethodType("card").build();
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "Chains.java"), []byte(chains), 0o644); err != nil {
		t.Fatal(err)
	}
	stmts := `import com.stripe.param.PaymentIntentCreateParams;

class Demo {
  void a() {
    PaymentIntentCreateParams.Builder paramsBuilder = PaymentIntentCreateParams.builder();
    paramsBuilder.addPaymentMethodType("card");
    paramsBuilder.build();
  }
  void b() {
    PaymentIntentCreateParams.Builder paramsBuilder = PaymentIntentCreateParams.builder();
    paramsBuilder.addPaymentMethodType("card");
    paramsBuilder.build();
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "Stmts.java"), []byte(stmts), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	chainsOut := mustRead(t, dir, "Chains.java")
	if n := strings.Count(chainsOut, "setAutomaticPaymentMethods"); n != 2 {
		t.Errorf("two distinct chains must each get a companion, got %d:\n%s", n, chainsOut)
	}
	if !strings.Contains(chainsOut, "SetupIntentCreateParams.AutomaticPaymentMethods") {
		t.Errorf("the SetupIntent chain must use the SetupIntent params class:\n%s", chainsOut)
	}
	stmtsOut := mustRead(t, dir, "Stmts.java")
	if n := strings.Count(stmtsOut, "setAutomaticPaymentMethods"); n != 2 {
		t.Errorf("same-named builders in different methods must each get a companion, got %d:\n%s", n, stmtsOut)
	}
}

func TestCompanionHalfMigratedFileStillInserts(t *testing.T) {
	// Review finding: the already-present check must be per call site, not
	// per file — a half-migrated file's remaining call still needs the insert.
	dir := t.TempDir()
	src := `const stripe = require('stripe')('sk_test_x');
async function a() {
  await stripe.paymentIntents.create({
    amount: 1099,
    currency: 'eur',
    automatic_payment_methods: {enabled: true},
  });
}
async function b() {
  await stripe.paymentIntents.create({
    amount: 2099,
    currency: 'eur',
    payment_method_types: ['card', 'link'],
  });
}
`
	if err := os.WriteFile(filepath.Join(dir, "half.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	if rep.Companion.Inserts != 1 {
		t.Errorf("the un-migrated call must get the insert, got %d", rep.Companion.Inserts)
	}
	out := mustRead(t, dir, "half.js")
	if n := strings.Count(out, "automatic_payment_methods"); n != 2 {
		t.Errorf("expected both calls to carry the companion, got %d:\n%s", n, out)
	}
}

func TestCompanionCreateOnlyOperations(t *testing.T) {
	// Review finding: automatic_payment_methods is CREATE-only — update,
	// confirm, and modify sites must be bare-removed, never companioned.
	dir := t.TempDir()
	files := map[string]string{
		"update.js": `const stripe = require('stripe')('sk_test_x');
await stripe.paymentIntents.update('pi_123', {
  payment_method_types: ['card', 'ideal'],
});
`,
		"confirm.py": `import stripe

stripe.PaymentIntent.confirm(
    "pi_123",
    payment_method_types=["card", "ideal"],
)
`,
		"update.go": `package main

import (
	"github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/paymentintent"
)

func main() {
	params := &stripe.PaymentIntentParams{
		PaymentMethodTypes: stripe.StringSlice([]string{"card", "ideal"}),
	}
	pi, _ := paymentintent.Update("pi_123", params)
	_ = pi
}
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AllClean {
		t.Fatal("expected reparse clean")
	}
	if rep.Companion.Inserts != 0 {
		t.Errorf("update/confirm sites must never gain the companion, got %d inserts", rep.Companion.Inserts)
	}
	for name := range files {
		out := mustRead(t, dir, name)
		if strings.Contains(out, "payment_method_types") || strings.Contains(out, "PaymentMethodTypes") {
			t.Errorf("%s: param must still be removed:\n%s", name, out)
		}
		if strings.Contains(out, "automatic_payment_methods") || strings.Contains(out, "AutomaticPaymentMethods") {
			t.Errorf("%s: create-only companion must not appear:\n%s", name, out)
		}
	}
}

func TestCompanionJavaSessionBuilderNotMatched(t *testing.T) {
	// Review finding: a Checkout Session builder mentioning
	// setPaymentIntentData must not be mistaken for a PaymentIntent create.
	dir := t.TempDir()
	src := `import com.stripe.param.checkout.SessionCreateParams;

class Demo {
  void go() {
    SessionCreateParams params = SessionCreateParams.builder().addPaymentMethodType("card").setPaymentIntentData(SessionCreateParams.PaymentIntentData.builder().build()).build();
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "Sess.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := fixRun(dir, dpmRule, true, false, &companionDecision{insert: true, reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Companion.Inserts != 0 {
		t.Errorf("session builder must not gain the companion, got %d inserts", rep.Companion.Inserts)
	}
	out := mustRead(t, dir, "Sess.java")
	if strings.Contains(out, "AutomaticPaymentMethods") {
		t.Errorf("no companion belongs in a Session builder:\n%s", out)
	}
}

func TestDecideCompanionUsesRuleCutoffAndConfigNote(t *testing.T) {
	// Review findings: the fork must be computed against THIS rule's cutoff
	// (not the dpm package constant), and must surface the missing-Dashboard-
	// config caveat the doctor raises on the same facts.
	rule := dpmRule
	rule.IntroducedIn = "2024-01-01"
	facts := &accountFacts{
		EventVersions: map[string]int{"2023-10-16": 5},
		OldestVersion: "2023-10-16",
		ConfiguredOK:  true,
	}
	// 2023-10-16 is >= the dpm constant but < this rule's cutoff: must insert.
	if dec := decideCompanion(rule, facts); !dec.insert {
		t.Errorf("traffic below the RULE's cutoff must insert, got %+v", dec)
	}
	facts.EventVersions = map[string]int{"2024-06-20": 3}
	facts.OldestVersion = "2024-06-20"
	if dec := decideCompanion(rule, facts); dec.insert {
		t.Errorf("traffic at/after the rule's cutoff must omit, got %+v", dec)
	}
	facts.ConfiguredOK = false
	if dec := decideCompanion(rule, facts); !strings.Contains(dec.reason, "no active Dashboard") {
		t.Errorf("missing Dashboard config must be surfaced in the reason, got %q", dec.reason)
	}
}

func TestRemovalLeavesNoBlankArtifacts(t *testing.T) {
	// The bed-A review finding: every deleted entry used to leave a
	// whitespace-only line. Full-line expansion must prevent that in every
	// pair-shaped language.
	names := []string{"pi.py", "pi.rb", "pi.js", "pi.php", "pi.go", "Pi.cs"}
	dir := copyFixtures(t, names...)
	before := map[string]int{}
	for _, n := range names {
		before[n] = strings.Count(mustRead(t, dir, n), "\n")
	}
	if _, err := fixRun(dir, dpmRule, true, false, nil); err != nil {
		t.Fatal(err)
	}
	for n, lines := range before {
		out := mustRead(t, dir, n)
		if wsOnlyLine.MatchString(out) {
			t.Errorf("%s: whitespace-only line left behind:\n%s", n, out)
		}
		if got := strings.Count(out, "\n"); got != lines-1 {
			t.Errorf("%s: expected exactly the param line to disappear (%d -> %d lines), got %d", n, lines, lines-1, got)
		}
	}
}
