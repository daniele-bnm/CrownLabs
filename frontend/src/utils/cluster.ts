import type { ApolloClient } from '@apollo/client';
import {
  ClusterKubeconfigDocument,
  EnvironmentType,
  Phase2,
  type ClusterKubeconfigQuery,
  type ClusterKubeconfigQueryVariables,
} from '../generated-types';

type EnvironmentKind = {
  environmentType?: EnvironmentType | null;
};

export const hasClusterEnvironment = (
  environments: readonly (EnvironmentKind | null | undefined)[],
): boolean =>
  environments.some(env => env?.environmentType === EnvironmentType.Cluster);

export const supportsEnvironmentSSH = (env: EnvironmentKind): boolean =>
  env.environmentType !== EnvironmentType.Cluster &&
  env.environmentType !== EnvironmentType.Container &&
  env.environmentType !== EnvironmentType.Standalone;

export const getEnvironmentWorkerCount = (
  env: EnvironmentKind & {
    cluster?: { workersCount?: number | null } | null;
  },
): number => {
  if (env.environmentType !== EnvironmentType.Cluster) return 1;

  const count = env.cluster?.workersCount;
  if (count == null || !Number.isInteger(count) || count < 1 || count > 10) {
    throw new Error(
      'Cluster environments require an integer worker count from 1 to 10.',
    );
  }
  return count;
};

type ConnectableEnvironment = EnvironmentKind & {
  phase?: Phase2 | null;
  guiEnabled?: boolean | null;
  clusterAccess?: { ready?: boolean | null } | null;
};

/** Whether the external access to the API server of a Cluster environment is ready. */
export const isClusterAccessReady = (env: ConnectableEnvironment): boolean =>
  env.environmentType === EnvironmentType.Cluster &&
  env.phase === Phase2.Ready &&
  env.clusterAccess?.ready === true;

/**
 * Whether the Connect action is available for an environment of an instance
 * including at least one Cluster environment: GUI/SSH for the other ready
 * environments, the API server access for the Cluster ones.
 */
export const canConnectToEnvironment = (
  env: ConnectableEnvironment,
): boolean =>
  env.environmentType === EnvironmentType.Cluster
    ? isClusterAccessReady(env)
    : env.phase === Phase2.Ready &&
      (!!env.guiEnabled || supportsEnvironmentSSH(env));

/** Name of the downloaded kubeconfig file of a Cluster environment. */
export const kubeconfigFileName = (
  instanceName: string,
  environmentName: string,
): string =>
  `${`${instanceName}-${environmentName}`.replace(/[^A-Za-z0-9._-]/g, '-')}.kubeconfig`;

/** Example kubectl command using the downloaded kubeconfig file. */
export const kubectlExampleCommand = (fileName: string): string =>
  `kubectl --kubeconfig ${fileName} get nodes`;

export const KUBECONFIG_SECRET_KEY = 'kubeconfig';

const decodeBase64 = (value: string): string => {
  const binary = atob(value);
  const bytes = Uint8Array.from(binary, c => c.charCodeAt(0));
  return new TextDecoder().decode(bytes);
};

/** Extracts the kubeconfig from the (base64-encoded) data of the dedicated Secret. */
export const decodeKubeconfigSecret = (
  data?: readonly { key: string; value: string }[] | null,
): string => {
  const entry = data?.find(e => e.key === KUBECONFIG_SECRET_KEY);
  if (!entry?.value) {
    throw new Error('The kubeconfig is not available yet, please retry later.');
  }
  try {
    return decodeBase64(entry.value);
  } catch {
    throw new Error('The kubeconfig retrieved from the cluster is not valid.');
  }
};

type KubeconfigQueryClient = Pick<ApolloClient<unknown>, 'query'>;

/**
 * Reads the kubeconfig of a Cluster environment from its dedicated Secret,
 * with the permissions of the current user. The result is never cached.
 */
export const fetchClusterKubeconfig = async (
  client: KubeconfigQueryClient,
  namespace: string,
  secretName: string,
): Promise<string> => {
  const { data, error, errors } = await client.query<
    ClusterKubeconfigQuery,
    ClusterKubeconfigQueryVariables
  >({
    query: ClusterKubeconfigDocument,
    variables: { namespace, name: secretName },
    fetchPolicy: 'no-cache',
    errorPolicy: 'none',
  });
  const firstError = error ?? errors?.[0];
  if (firstError) throw new Error(firstError.message);
  return decodeKubeconfigSecret(data?.secret?.data);
};
