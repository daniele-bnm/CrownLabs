import { type FC, useState } from 'react';
import { Alert, Button, Space, Typography } from 'antd';
import { DownloadOutlined } from '@ant-design/icons';
import { useApolloClient } from '@apollo/client';
import type { InstanceEnvironment } from '../../../../utils';
import {
  fetchClusterKubeconfig,
  isClusterAccessReady,
  kubeconfigFileName,
  kubectlExampleCommand,
} from '../../../../utils/cluster';

const { Text } = Typography;

export interface IClusterAccessModalContentProps {
  instanceName: string;
  namespace: string;
  environment: InstanceEnvironment;
}

const saveFile = (content: string, fileName: string) => {
  const url = URL.createObjectURL(
    new Blob([content], { type: 'application/yaml' }),
  );
  const link = document.createElement('a');
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);
};

const ClusterAccessModalContent: FC<IClusterAccessModalContentProps> = ({
  instanceName,
  namespace,
  environment,
}) => {
  const client = useApolloClient();
  const [downloading, setDownloading] = useState(false);
  const [error, setError] = useState<string>();

  const { clusterAccess } = environment;
  const ready = isClusterAccessReady(environment);
  const fileName = kubeconfigFileName(instanceName, environment.name);

  const handleDownload = async () => {
    if (!clusterAccess?.kubeconfigSecretName) return;
    setDownloading(true);
    setError(undefined);
    try {
      // The Secret is read only on demand, without caching its content.
      const kubeconfig = await fetchClusterKubeconfig(
        client,
        namespace,
        clusterAccess.kubeconfigSecretName,
      );
      saveFile(kubeconfig, fileName);
    } catch (e) {
      setError(
        e instanceof Error ? e.message : 'Unable to download the kubeconfig',
      );
    } finally {
      setDownloading(false);
    }
  };

  return (
    <Space
      direction="vertical"
      className="flex justify-center text-center max-w-xl mx-auto"
    >
      <Text className="text-base">
        Use <Text code>kubectl</Text> from your computer to access the
        Kubernetes cluster of the environment{' '}
        <Text strong>{environment.name}</Text>.
      </Text>

      <div className="flex flex-col items-center gap-1 mt-2">
        <Text strong>API server endpoint</Text>
        <Text
          type="warning"
          code
          copyable={!!clusterAccess?.endpoint}
          className="break-all"
        >
          {clusterAccess?.endpoint ?? 'unknown'}
        </Text>
      </div>

      <Button
        className="mt-4"
        type="primary"
        shape="round"
        icon={<DownloadOutlined />}
        loading={downloading}
        disabled={!ready || !clusterAccess?.kubeconfigSecretName}
        onClick={handleDownload}
      >
        Download kubeconfig
      </Button>

      {error && (
        <Alert
          type="error"
          showIcon
          closable
          onClose={() => setError(undefined)}
          message="Unable to download the kubeconfig"
          description={error}
          className="text-left"
        />
      )}

      <div className="border-t border-gray-400 w-full mt-4" />

      <Text className="mt-4 text-base">
        Then, from the folder containing the downloaded file, run for example:
      </Text>
      <Text type="warning" code copyable className="flex justify-center">
        {kubectlExampleCommand(fileName)}
      </Text>
      <Text className="text-sm text-gray-500">
        The kubeconfig grants administrative access to your cluster: keep it
        private.
      </Text>
    </Space>
  );
};

export default ClusterAccessModalContent;
