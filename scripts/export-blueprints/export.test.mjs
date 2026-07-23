import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const tsxCLI = require.resolve('tsx/cli');

function workbenchBlueprint(node = {}) {
  return {
    title: { defaultMessage: 'Subscription integration' },
    listViewDescription: { defaultMessage: 'Add billing to an application.' },
    blueprintType: 'learning',
    chapters: [{
      key: 'checkout',
      title: { defaultMessage: 'Checkout' },
      nodes: [{
        type: 'apiRequest',
        key: 'create-checkout',
        title: { defaultMessage: 'Create Checkout' },
        description: 'Persist identity and authorize access after Checkout.',
        request: {
          method: 'POST',
          path: '/v1/checkout/sessions',
          params: { mode: 'subscription' },
        },
        ...node,
      }],
    }],
  };
}

function runExporter(files) {
  const root = mkdtempSync(join(tmpdir(), 'stripe-blueprint-export-'));
  const source = join(root, 'source');
  const out = join(root, 'out');
  mkdirSync(source);
  mkdirSync(out);
  for (const [name, blueprint] of Object.entries(files)) {
    writeFileSync(join(source, `${name}.json`), `${JSON.stringify(blueprint, null, 2)}\n`);
  }
  const result = spawnSync(
    process.execPath,
    [tsxCLI, join(scriptDir, 'export.ts'), '--source', source, '--out', out],
    { cwd: scriptDir, encoding: 'utf8' },
  );
  return {
    out,
    result,
    cleanup: () => rmSync(root, { recursive: true, force: true }),
  };
}

function readOutput(out, name) {
  return JSON.parse(readFileSync(join(out, `${name}.json`), 'utf8'));
}

test('preserves camelCase and snake_case lifecycle contracts without synthesizing them', t => {
  const camel = workbenchBlueprint({
    requiredOutcomes: [{
      id: 'server_authorized_access',
      factRefs: ['redirect_not_proof'],
      statement: { defaultMessage: 'Authorize access from persisted subscription state.' },
    }],
  });
  camel.lifecycleFacts = [{
    id: 'redirect_not_proof',
    statement: { defaultMessage: 'A redirect is not proof of subscription state.' },
  }];

  const snake = workbenchBlueprint({
    required_outcomes: [{
      id: 'durable_customer_mapping',
      fact_refs: ['customer_identity'],
      statement: 'Persist the application principal to Stripe Customer mapping.',
    }],
  });
  snake.lifecycle_facts = [{
    id: 'customer_identity',
    statement: 'Customer email is not a stable application identity key.',
  }];

  const run = runExporter({
    camel,
    snake,
    absent: workbenchBlueprint(),
  });
  t.after(run.cleanup);

  assert.equal(run.result.status, 0, run.result.stderr);
  assert.deepEqual(readOutput(run.out, 'camel').lifecycle_facts, [{
    id: 'redirect_not_proof',
    statement: 'A redirect is not proof of subscription state.',
  }]);
  assert.deepEqual(readOutput(run.out, 'camel').steps[0].nodes[0].required_outcomes, [{
    id: 'server_authorized_access',
    fact_refs: ['redirect_not_proof'],
    statement: 'Authorize access from persisted subscription state.',
  }]);
  assert.deepEqual(readOutput(run.out, 'snake').lifecycle_facts, [{
    id: 'customer_identity',
    statement: 'Customer email is not a stable application identity key.',
  }]);
  assert.deepEqual(readOutput(run.out, 'snake').steps[0].nodes[0].required_outcomes, [{
    id: 'durable_customer_mapping',
    fact_refs: ['customer_identity'],
    statement: 'Persist the application principal to Stripe Customer mapping.',
  }]);

  const absent = readOutput(run.out, 'absent');
  assert.equal(Object.hasOwn(absent, 'lifecycle_facts'), false);
  assert.equal(Object.hasOwn(absent.steps[0].nodes[0], 'required_outcomes'), false);
});

test('rejects root aliases declared in both casing styles and exits nonzero after processing', t => {
  const invalid = workbenchBlueprint();
  invalid.lifecycleFacts = [];
  invalid.lifecycle_facts = [];

  const run = runExporter({
    valid: workbenchBlueprint(),
    invalid,
  });
  t.after(run.cleanup);

  assert.notEqual(run.result.status, 0);
  assert.match(run.result.stderr, /declares both "lifecycleFacts" and "lifecycle_facts"/);
  assert.match(run.result.stderr, /Failed to export 1 blueprint file/);
  assert.equal(Object.hasOwn(readOutput(run.out, 'valid'), 'lifecycle_facts'), false);
});

test('rejects node outcome aliases declared in both casing styles', t => {
  const invalid = workbenchBlueprint({
    requiredOutcomes: [],
    required_outcomes: [],
  });
  const run = runExporter({ invalid });
  t.after(run.cleanup);

  assert.notEqual(run.result.status, 0);
  assert.match(run.result.stderr, /declares both "requiredOutcomes" and "required_outcomes"/);
  assert.match(run.result.stderr, /Failed to export 1 blueprint file/);
});
