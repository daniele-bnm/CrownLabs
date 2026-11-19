const assert = require('node:assert/strict');
const { test } = require('node:test');
const { graphql } = require('graphql');
const { createSecretSchema } = require('../src/secrets');
const { joinSchemas } = require('../src/schema');

const query = `query($namespace: String!, $name: String!) {
  secret: readSecretByName(namespace: $namespace, name: $name) {
    name namespace type data { key value }
  }
}`;

function fakeFetch(status, body) {
  const calls = [];
  const impl = async (url, options) => {
    calls.push({ url, options });
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  };
  return { calls, impl };
}

const secret = {
  kind: 'Secret',
  type: 'Opaque',
  metadata: { name: 'inst-cluster-tenant-kubeconfig', namespace: 'tenant-john-doe' },
  data: { kubeconfig: Buffer.from('apiVersion: v1').toString('base64') },
};
const variables = { namespace: 'tenant-john-doe', name: 'inst-cluster-tenant-kubeconfig' };

async function run(schema, contextValue, vars = variables) {
  return graphql({
    schema, source: query, variableValues: vars, contextValue,
  });
}

test('forwards only the user token and returns the secret data', async () => {
  const { calls, impl } = fakeFetch(200, secret);
  const schema = joinSchemas([{ schema: createSecretSchema({ kubeApiUrl: 'https://k8s/', fetchImpl: impl }) }]);
  const res = await run(schema, { token: 'user-token' });

  assert.equal(res.errors, undefined);
  assert.deepEqual(res.data.secret.data.map((entry) => ({ ...entry })), [{ key: 'kubeconfig', value: secret.data.kubeconfig }]);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'https://k8s/api/v1/namespaces/tenant-john-doe/secrets/inst-cluster-tenant-kubeconfig');
  assert.equal(calls[0].options.method, 'GET');
  assert.deepEqual(calls[0].options.headers, { Authorization: 'Bearer user-token', Accept: 'application/json' });
});

test('never falls back to other credentials without a user token', async () => {
  const { calls, impl } = fakeFetch(200, secret);
  const schema = createSecretSchema({ kubeApiUrl: 'https://k8s', fetchImpl: impl });
  for (const context of [{}, { token: '' }, null]) {
    const res = await run(schema, context);
    assert.equal(res.data.secret, null);
    assert.equal(res.errors[0].extensions.code, 'UNAUTHENTICATED');
  }
  assert.equal(calls.length, 0);
});

test('is disabled when the API server would not authenticate the user token', async () => {
  const { calls, impl } = fakeFetch(200, secret);
  const schema = createSecretSchema({ kubeApiUrl: 'http://localhost:8001', enabled: false, fetchImpl: impl });
  const res = await run(schema, { token: 'user-token' });
  assert.equal(res.errors[0].extensions.code, 'NOT_SUPPORTED');
  assert.equal(calls.length, 0);
});

test('propagates the Kubernetes RBAC denial', async () => {
  const { impl } = fakeFetch(403, { kind: 'Status', message: 'secrets "x" is forbidden' });
  const schema = createSecretSchema({ kubeApiUrl: 'https://k8s', fetchImpl: impl });
  const res = await run(schema, { token: 'user-token' });
  assert.equal(res.data.secret, null);
  assert.equal(res.errors[0].message, 'secrets "x" is forbidden');
  assert.equal(res.errors[0].extensions.code, 'FORBIDDEN');
});

test('rejects invalid names without contacting the API server', async () => {
  const { calls, impl } = fakeFetch(200, secret);
  const schema = createSecretSchema({ kubeApiUrl: 'https://k8s', fetchImpl: impl });
  for (const vars of [
    { namespace: 'tenant', name: '../../secrets' },
    { namespace: 'a/b', name: 'secret' },
    { namespace: 'tenant', name: '' },
  ]) {
    const res = await run(schema, { token: 'user-token' }, vars);
    assert.equal(res.errors[0].extensions.code, 'BAD_USER_INPUT');
  }
  assert.equal(calls.length, 0);
});

test('exposes only the read of a single secret', () => {
  const schema = createSecretSchema({ kubeApiUrl: 'https://k8s', fetchImpl: async () => null });
  assert.deepEqual(Object.keys(schema.getQueryType().getFields()), ['readSecretByName']);
  assert.equal(schema.getMutationType(), undefined);
  assert.equal(schema.getSubscriptionType(), undefined);
});
