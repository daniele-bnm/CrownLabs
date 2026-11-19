import assert from 'node:assert/strict';
import { before, after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';

let server;
let api;
let cluster;
let formUtils;
let mapping;
let quotas;
let documents;

before(async () => {
  globalThis.window = new Proxy(
    { VITE_APP_CROWNLABS_IMAGELIST_CLUSTER_FLAVORS: 'local-clusterflavors' },
    {
      get: (target, key) =>
        target[key] ?? (key === 'VITE_APP_CUSTOM_RESOURCES' ? '{}' : 'test'),
    },
  );
  server = await createServer({
    root: fileURLToPath(new URL('../', import.meta.url)),
    configFile: false,
    envFile: false,
    cacheDir: 'node_modules/.vite-cluster-tests',
    ssr: { noExternal: ['@apollo/client'] },
    server: { middlewareMode: true, hmr: false, watch: null },
  });
  api = await server.ssrLoadModule('/src/generated-types.tsx');
  cluster = await server.ssrLoadModule('/src/utils/cluster.ts');
  formUtils = await server.ssrLoadModule(
    '/src/components/workspaces/ModalCreateTemplate/utils.ts',
  );
  mapping = await server.ssrLoadModule('/src/utilsLogic.tsx');
  quotas = await server.ssrLoadModule('/src/utils/quota.ts');
  documents = await server.ssrLoadModule(
    '/src/graphql-components/subscription/index.ts',
  );
});

after(async () => {
  await server?.close();
  delete globalThis.window;
});

const environment = (workersCount = 3) => ({
  name: 'wl-cluster',
  environmentType: api.EnvironmentType.Cluster,
  cluster: { workersCount },
  image: 'k8s-1-30-basic',
  guiEnabled: false,
  persistent: false,
  resources: { cpu: 2, memory: '2Gi', reservedCPUPercentage: 50 },
});

const template = envs => ({
  metadata: { name: 'cluster-template', namespace: 'workspace-test' },
  spec: {
    prettyName: 'Cluster template',
    environmentList: envs,
    workspaceCrownlabsPolitoItWorkspaceRef: { name: 'test' },
  },
});

const instance = envs => ({
  metadata: { name: 'cluster-instance', namespace: 'tenant-test' },
  spec: {
    running: true,
    templateCrownlabsPolitoItTemplateRef: {
      name: 'cluster-template',
      namespace: 'workspace-test',
      templateWrapper: { itPolitoCrownlabsV1alpha2Template: template(envs) },
    },
  },
  status: {
    phase: api.Phase2.Ready,
    environments: envs.map(env => ({
      name: env.name,
      phase: api.Phase2.Ready,
    })),
  },
});

test('cluster catalogs retain exact, untagged names and distinguish missing from empty', () => {
  const catalog = images => ({
    name: 'local-clusterflavors',
    registryName: 'central-flavors',
    images,
  });
  assert.deepEqual(
    formUtils.getClusterFlavorNames(
      catalog([
        { name: 'z-flavor', versions: [] },
        { name: 'a-flavor', versions: [] },
        { name: 'a-flavor', versions: [] },
      ]),
    ),
    ['a-flavor', 'z-flavor'],
  );
  assert.deepEqual(formUtils.getClusterFlavorNames(catalog([])), []);
  assert.equal(formUtils.getClusterFlavorNames(undefined), undefined);
});

test('worker count accepts boundaries and rejects missing, fractional, and out-of-range values', () => {
  for (const count of [1, 3, 10]) {
    assert.equal(cluster.getEnvironmentWorkerCount(environment(count)), count);
  }
  for (const count of [0, 11, 1.5, null, undefined, NaN]) {
    assert.throws(
      () =>
        cluster.getEnvironmentWorkerCount({
          ...environment(),
          cluster: { workersCount: count },
        }),
      /integer worker count/,
    );
  }
  assert.throws(
    () =>
      cluster.getEnvironmentWorkerCount({
        environmentType: api.EnvironmentType.Cluster,
      }),
    /integer worker count/,
  );
  assert.equal(
    cluster.getEnvironmentWorkerCount({
      environmentType: api.EnvironmentType.VirtualMachine,
    }),
    1,
  );
});

test('cluster form normalization preserves the flavor and count and clears unsupported VM options', () => {
  const normalized = formUtils.normalizeClusterEnvironment({
    ...environment(),
    cpu: 2,
    ram: 2,
    gui: true,
    persistent: true,
    disk: 20,
    reservedCpu: 50,
    registry: '',
    rewriteUrl: true,
    sharedVolumeMounts: [{ sharedVolume: 'test/shared', mountPath: '/shared' }],
    otherResources: { nvidiaComGpu: 1 },
  });
  assert.equal(normalized.image, 'k8s-1-30-basic');
  assert.deepEqual(normalized.cluster, { workersCount: 3 });
  assert.equal(normalized.gui, false);
  assert.equal(normalized.persistent, false);
  assert.equal(normalized.mountMyDriveVolume, false);
  assert.equal(normalized.disk, 0);
  assert.deepEqual(normalized.sharedVolumeMounts, []);
  assert.deepEqual(normalized.otherResources, {});
  const vm = formUtils.normalizeClusterEnvironment({
    ...normalized,
    environmentType: api.EnvironmentType.VirtualMachine,
  });
  assert.equal(Object.hasOwn(vm, 'cluster'), false);
});

test('templates and instances account for every worker without changing VM accounting', () => {
  for (const workers of [1, 3, 10]) {
    const envs = [environment(workers)];
    const guiTemplate = mapping.makeGuiTemplate({
      original: template(envs),
      alias: { id: 'cluster-template', name: 'Cluster template' },
    });
    const guiInstance = mapping.makeGuiInstance(instance(envs));
    assert.equal(guiTemplate.resources.cpu, workers * 2);
    assert.equal(guiInstance.resources.cpu, workers * 2);
    assert.equal(guiInstance.resources.memory, workers * 2);
    assert.deepEqual(guiTemplate.environmentList[0].cluster, {
      workersCount: workers,
    });
    assert.deepEqual(guiInstance.environments[0].cluster, {
      workersCount: workers,
    });
    assert.equal(guiTemplate.environmentList[0].resources.cpu, 2);
  }
  const vm = {
    name: 'vm-env',
    environmentType: api.EnvironmentType.VirtualMachine,
    resources: { cpu: 1, memory: '4Gi', disk: '10Gi' },
    persistent: true,
    guiEnabled: true,
  };
  const guiInstance = mapping.makeGuiInstance(instance([environment(), vm]));
  assert.deepEqual(guiInstance.resources, {
    cpu: 7,
    memory: 10,
    disk: 10,
    otherResources: {},
  });
  const consumed = quotas.calculateWorkspaceConsumedQuota([guiInstance]);
  assert.equal(consumed.test.instances, 1);
  assert.equal(consumed.test.cpu, 7);
  assert.equal(consumed.test.memory, 10);
  const available = quotas.calculateAvailableQuota(
    { test: { instances: 1, cpu: 7, memory: 10, disk: 10 } },
    consumed,
  );
  assert.equal(available.test.instances, 0);
  assert.equal(available.test.cpu, 0);
  assert.equal(available.test.memory, 0);
});

test('cluster presence is detected in mixed environments and SSH remains available only to VMs', () => {
  const vm = { environmentType: api.EnvironmentType.VirtualMachine };
  const envs = [vm, environment()];
  assert.equal(cluster.hasClusterEnvironment(envs), true);
  assert.equal(cluster.hasClusterEnvironment([vm]), false);
  assert.equal(cluster.supportsEnvironmentSSH(environment()), false);
  assert.equal(cluster.supportsEnvironmentSSH(vm), true);
  assert.equal(
    cluster.supportsEnvironmentSSH({
      environmentType: api.EnvironmentType.Container,
    }),
    false,
  );
  assert.equal(
    cluster.supportsEnvironmentSSH({
      environmentType: api.EnvironmentType.Standalone,
    }),
    false,
  );
  const guiInstance = mapping.makeGuiInstance(
    instance([
      {
        ...environment(),
        guiEnabled: true,
        persistent: true,
      },
    ]),
  );
  assert.equal(guiInstance.gui, false);
  assert.equal(guiInstance.persistent, false);
  assert.equal(guiInstance.allowPublicExposure, false);
});

test('launch quota checks require all worker resources at the exact threshold', () => {
  const guiTemplate = mapping.makeGuiTemplate({
    original: template([environment(3)]),
    alias: { id: 'cluster-template', name: 'Cluster template' },
  });
  const available = { instances: 1, cpu: 6, memory: 6, disk: 0 };
  assert.equal(quotas.canCreateInstance(guiTemplate, available), true);
  assert.equal(
    quotas.canCreateInstance(guiTemplate, { ...available, cpu: 5 }),
    false,
  );
  assert.equal(
    quotas.canCreateInstance(guiTemplate, { ...available, memory: 5 }),
    false,
  );
  assert.equal(
    quotas.canCreateInstance(guiTemplate, { ...available, instances: 0 }),
    false,
  );
});

test('live subscription documents include the shared cluster environment fragment', () => {
  for (const document of [
    documents.updatedWorkspaceTemplates,
    documents.updatedOwnedInstances,
    documents.updatedInstancesLabelSelector,
    api.CreateTemplateDocument,
    api.CreateInstanceDocument,
    api.ApplyTemplateJsonPatchDocument,
  ]) {
    const fragment = document.definitions.find(
      definition =>
        definition.kind === 'FragmentDefinition' &&
        definition.name.value === 'TemplateEnvironmentFields',
    );
    assert.ok(fragment);
    const clusterField = fragment.selectionSet.selections.find(
      selection =>
        selection.kind === 'Field' && selection.name.value === 'cluster',
    );
    assert.equal(
      clusterField.selectionSet.selections[0].name.value,
      'workersCount',
    );
    for (const name of ['image', 'environmentType', 'resources']) {
      assert.ok(
        fragment.selectionSet.selections.some(
          selection =>
            selection.kind === 'Field' && selection.name.value === name,
        ),
      );
    }
  }
});

const clusterAccess = (ready = true) => ({
  endpoint: 'https://inst-wl-cluster-0123456789ab.k8s.crownlabs.local:9443',
  kubeconfigSecretName: 'cluster-instance-wl-cluster-tenant-kubeconfig',
  ready,
});

const instanceWithAccess = (envs, access) => {
  const raw = instance(envs);
  raw.status.environments = raw.status.environments.map(env => ({
    ...env,
    clusterAccess: access[env.name],
  }));
  return raw;
};

test('cluster access status is mapped only for cluster environments', () => {
  const vm = {
    name: 'vm-env',
    environmentType: api.EnvironmentType.VirtualMachine,
    resources: { cpu: 1, memory: '1Gi', disk: '10Gi' },
    guiEnabled: true,
  };
  const gui = mapping.makeGuiInstance(
    instanceWithAccess([environment(), vm], {
      'wl-cluster': clusterAccess(),
      'vm-env': clusterAccess(),
    }),
  );
  assert.deepEqual(gui.environments[0].clusterAccess, clusterAccess());
  assert.equal(gui.environments[1].clusterAccess, undefined);
  const missing = mapping.makeGuiInstance(instance([environment()]));
  assert.equal(missing.environments[0].clusterAccess, undefined);
});

test('connect is enabled for clusters only when the access is ready, keeping VM behavior', () => {
  const ready = api.Phase2.Ready;
  const cl = (phase, access) => ({
    environmentType: api.EnvironmentType.Cluster,
    phase,
    clusterAccess: access,
  });
  assert.equal(
    cluster.canConnectToEnvironment(cl(ready, clusterAccess())),
    true,
  );
  assert.equal(
    cluster.canConnectToEnvironment(cl(ready, clusterAccess(false))),
    false,
  );
  assert.equal(cluster.canConnectToEnvironment(cl(ready, undefined)), false);
  assert.equal(
    cluster.canConnectToEnvironment(cl(api.Phase2.Running, clusterAccess())),
    false,
  );
  const vm = phase => ({
    environmentType: api.EnvironmentType.VirtualMachine,
    phase,
  });
  assert.equal(cluster.canConnectToEnvironment(vm(ready)), true);
  assert.equal(cluster.canConnectToEnvironment(vm(api.Phase2.Off)), false);
  assert.equal(
    cluster.canConnectToEnvironment({
      environmentType: api.EnvironmentType.Container,
      phase: ready,
      guiEnabled: false,
    }),
    false,
  );
});

test('the kubeconfig is read on demand, without cache, and decoded', async () => {
  const content = 'apiVersion: v1\nkind: Config\n# àccénts\n';
  const value = Buffer.from(content, 'utf8').toString('base64');
  const calls = [];
  const client = {
    query: async options => {
      calls.push(options);
      return { data: { secret: { data: [{ key: 'kubeconfig', value }] } } };
    },
  };
  const result = await cluster.fetchClusterKubeconfig(
    client,
    'tenant-test',
    'secret-name',
  );
  assert.equal(result, content);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].fetchPolicy, 'no-cache');
  assert.equal(calls[0].query, api.ClusterKubeconfigDocument);
  assert.deepEqual(calls[0].variables, {
    namespace: 'tenant-test',
    name: 'secret-name',
  });
});

test('kubeconfig errors are reported to the user', async () => {
  const failing = {
    query: async () => {
      throw new Error('secrets "x" is forbidden');
    },
  };
  await assert.rejects(
    cluster.fetchClusterKubeconfig(failing, 'ns', 'x'),
    /forbidden/,
  );
  const withErrors = {
    query: async () => ({ data: null, errors: [{ message: 'not found' }] }),
  };
  await assert.rejects(
    cluster.fetchClusterKubeconfig(withErrors, 'ns', 'x'),
    /not found/,
  );
  const empty = { query: async () => ({ data: { secret: { data: [] } } }) };
  await assert.rejects(
    cluster.fetchClusterKubeconfig(empty, 'ns', 'x'),
    /not available/,
  );
});

test('kubeconfig file name and kubectl example are consistent', () => {
  const fileName = cluster.kubeconfigFileName('my.instance', 'wl-cluster');
  assert.equal(fileName, 'my.instance-wl-cluster.kubeconfig');
  assert.equal(
    cluster.kubectlExampleCommand(fileName),
    'kubectl --kubeconfig my.instance-wl-cluster.kubeconfig get nodes',
  );
  assert.equal(
    cluster.kubeconfigFileName('a b/c', 'env'),
    'a-b-c-env.kubeconfig',
  );
});

test('subscription updates are applied when only the cluster access changes', () => {
  const before = mapping.makeGuiInstance(
    instanceWithAccess([environment()], {
      'wl-cluster': clusterAccess(false),
    }),
  );
  const after = mapping.makeGuiInstance(
    instanceWithAccess([environment()], { 'wl-cluster': clusterAccess() }),
  );
  assert.equal(
    mapping.getSubObjTypeCustom(before, after, api.UpdateType.Modified),
    mapping.SubObjType.UpdatedInfo,
  );
  assert.equal(
    mapping.getSubObjTypeCustom(after, after, api.UpdateType.Modified),
    mapping.SubObjType.Drop,
  );
  const raw = ready =>
    instanceWithAccess([environment()], {
      'wl-cluster': clusterAccess(ready),
    });
  assert.equal(
    mapping.getSubObjTypeK8s(raw(false), raw(true), api.UpdateType.Modified),
    mapping.SubObjType.UpdatedInfo,
  );
});

test('owned instance documents request the cluster access status', () => {
  for (const document of [
    api.OwnedInstancesDocument,
    documents.updatedOwnedInstances,
  ]) {
    const source = document.loc.source.body;
    assert.match(
      source,
      /clusterAccess\s*{\s*endpoint\s+kubeconfigSecretName\s+ready\s*}/,
    );
    assert.doesNotMatch(source, /readSecretByName/);
  }
});

test('connect button and cluster modal render the access information', async () => {
  // React is externalized by the SSR loader, hence it is shared with the components.
  const React = await import('react');
  const { renderToStaticMarkup } = await import('react-dom/server');
  const { ApolloClient, ApolloProvider, InMemoryCache } =
    await server.ssrLoadModule('@apollo/client');
  const { default: Modal } = await server.ssrLoadModule(
    '/src/components/activePage/TableInstance/ClusterAccessModalContent/ClusterAccessModalContent.tsx',
  );
  const { default: Default } = await server.ssrLoadModule(
    '/src/components/activePage/TableInstance/RowInstanceActions/RowInstanceActionsDefault.tsx',
  );
  const utils = await server.ssrLoadModule('/src/utils.ts');
  const client = new ApolloClient({
    cache: new InMemoryCache(),
    devtools: { enabled: false },
  });
  const render = element =>
    renderToStaticMarkup(
      React.createElement(ApolloProvider, { client }, element),
    );

  const gui = ready =>
    mapping.makeGuiInstance(
      instanceWithAccess([environment()], {
        'wl-cluster': clusterAccess(ready),
      }),
    );
  const modal = ready =>
    render(
      React.createElement(Modal, {
        instanceName: 'cluster-instance',
        namespace: 'tenant-test',
        environment: gui(ready).environments[0],
      }),
    );
  const readyHtml = modal(true);
  assert.match(readyHtml, /wl-cluster/);
  assert.match(
    readyHtml,
    /inst-wl-cluster-0123456789ab\.k8s\.crownlabs\.local:9443/,
  );
  assert.match(
    readyHtml,
    /kubectl --kubeconfig cluster-instance-wl-cluster\.kubeconfig get nodes/,
  );
  assert.doesNotMatch(
    readyHtml,
    /<button[^>]*disabled[^>]*>(?:(?!<\/button>).)*Download kubeconfig/s,
  );
  assert.match(
    modal(false),
    /<button[^>]*disabled[^>]*>(?:(?!<\/button>).)*Download kubeconfig/s,
  );

  const connect = ready =>
    render(
      React.createElement(Default, {
        extended: true,
        instance: gui(ready),
        viewMode: utils.WorkspaceRole.user,
        setSshModal: () => {},
        onClusterConnect: () => {},
      }),
    );
  const connectButtons = html => [
    ...html.matchAll(/<button[^>]*>(?:(?!<\/button>).)*?Connect<\/span>/gs),
  ];
  const enabled = connectButtons(connect(true));
  assert.ok(enabled.length > 0);
  assert.ok(enabled.every(([b]) => !/disabled=""/.test(b)));
  const disabled = connectButtons(connect(false));
  assert.ok(disabled.length > 0);
  assert.ok(disabled.every(([b]) => /disabled=""/.test(b)));
});
