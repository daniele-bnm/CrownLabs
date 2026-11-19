const { makeExecutableSchema } = require('@graphql-tools/schema');
const { GraphQLError } = require('graphql');
const { logger } = require('./utils');

// Kubernetes object names (DNS-1123 subdomain) and namespaces (DNS-1123 label).
const subdomainRegex = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
const labelRegex = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

const typeDefs = `
type Query {
  """
  Reads a single Secret by name. The request is performed with the token of the
  requesting user, hence it is authorized by the Kubernetes RBAC only.
  """
  readSecretByName(namespace: String!, name: String!): SecretByName
}
type SecretByName {
  name: String!
  namespace: String!
  type: String
  "The Secret data, base64-encoded as returned by the Kubernetes API."
  data: [SecretDataEntry!]!
}
type SecretDataEntry {
  key: String!
  value: String!
}`;

function gqlError(message, code, status) {
  return new GraphQLError(message, {
    extensions: { code, http: { status } },
  });
}

function errorCode(status) {
  switch (status) {
    case 401: return 'UNAUTHENTICATED';
    case 403: return 'FORBIDDEN';
    case 404: return 'NOT_FOUND';
    default: return 'KUBERNETES_API_ERROR';
  }
}

/**
 * Creates the schema allowing to read a single Secret by name.
 * The request is always forwarded with the token of the requesting user, never
 * with the credentials of qlkube, so that the Kubernetes RBAC is enforced.
 *
 * @param {object} options
 * @param {string} options.kubeApiUrl the URL of the Kubernetes API server
 * @param {boolean} options.enabled whether the API server authenticates the forwarded
 *   token (false, e.g., behind `kubectl proxy`, which would use its own credentials)
 * @param {Function} options.fetchImpl the fetch implementation (for testing purposes)
 */
function createSecretSchema({ kubeApiUrl, enabled = true, fetchImpl = fetch }) {
  const readSecretByName = async (_root, { namespace, name }, context) => {
    if (!enabled) {
      throw gqlError('Reading secrets is not available in this configuration', 'NOT_SUPPORTED', 501);
    }
    const token = context?.token;
    if (!token) {
      throw gqlError('Token Error! Token absent!', 'UNAUTHENTICATED', 401);
    }
    if (!labelRegex.test(namespace) || namespace.length > 63
      || !subdomainRegex.test(name) || name.length > 253) {
      throw gqlError('Invalid secret name or namespace', 'BAD_USER_INPUT', 400);
    }

    const url = `${kubeApiUrl.replace(/\/$/, '')}/api/v1/namespaces/${encodeURIComponent(namespace)}/secrets/${encodeURIComponent(name)}`;
    let res;
    try {
      res = await fetchImpl(url, {
        method: 'GET',
        headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
      });
    } catch (e) {
      logger.error({ namespace, name, error: e.message }, 'Secret read request failed');
      throw gqlError('Unable to contact the Kubernetes API server', 'KUBERNETES_API_ERROR', 502);
    }

    let body = null;
    try {
      body = await res.json();
    } catch (_) { /* empty: handled below */ }

    if (!res.ok) {
      logger.info({ namespace, name, status: res.status }, 'Secret read denied or failed');
      throw gqlError(body?.message || `Unable to read secret ${namespace}/${name}`, errorCode(res.status), res.status);
    }
    if (!body || body.kind !== 'Secret') {
      throw gqlError('Unexpected response from the Kubernetes API server', 'KUBERNETES_API_ERROR', 502);
    }

    return {
      name: body.metadata?.name ?? name,
      namespace: body.metadata?.namespace ?? namespace,
      type: body.type,
      data: Object.entries(body.data ?? {}).map(([key, value]) => ({ key, value })),
    };
  };

  return makeExecutableSchema({
    typeDefs,
    resolvers: { Query: { readSecretByName } },
  });
}

module.exports = { createSecretSchema };
