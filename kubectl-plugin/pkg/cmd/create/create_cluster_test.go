package create

import (
	"context"
	"testing"

	"github.com/ray-project/kuberay/kubectl-plugin/pkg/util/client"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	kubefake "k8s.io/client-go/kubernetes/fake"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	rayClientFake "github.com/ray-project/kuberay/ray-operator/pkg/client/clientset/versioned/fake"
)

func TestRayCreateClusterComplete(t *testing.T) {
	testStreams, _, _, _ := genericclioptions.NewTestIOStreams()
	cmdFactory := cmdutil.NewFactory(genericclioptions.NewConfigFlags(true))
	fakeCreateClusterOptions := NewCreateClusterOptions(cmdFactory, testStreams)
	fakeArgs := []string{"testRayClusterName"}
	cmd := &cobra.Command{Use: "cluster"}
	cmd.Flags().StringVarP(&fakeCreateClusterOptions.namespace, "namespace", "n", "", "")

	err := fakeCreateClusterOptions.Complete(cmd, fakeArgs)
	require.NoError(t, err)
	assert.Equal(t, "default", fakeCreateClusterOptions.namespace)
	assert.Equal(t, "testRayClusterName", fakeCreateClusterOptions.clusterName)
}

func TestRayCreateClusterValidate(t *testing.T) {
	cmdFactory := cmdutil.NewFactory(genericclioptions.NewConfigFlags(true))

	tests := []struct {
		name        string
		opts        *CreateClusterOptions
		expectError string
	}{
		{
			name: "should error when a resource quantity is invalid",
			opts: &CreateClusterOptions{
				cmdFactory: cmdFactory,
				headCPU:    "1",
				headMemory: "softmax",
			},
			expectError: "head-memory is not a valid resource quantity: quantities must match the regular expression '^([+-]?[0-9.]+)([eEinumkKMGTP]*[-+]?[0-9]*)$'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.expectError != "" {
				require.EqualError(t, err, tc.expectError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRayClusterCreateClusterRun(t *testing.T) {
	namespace := "namespace-1"
	clusterName := "cluster-1"
	cmdFactory := cmdutil.NewFactory(genericclioptions.NewConfigFlags(true))

	options := CreateClusterOptions{
		cmdFactory:   cmdFactory,
		clusterName:  clusterName,
		headCPU:      "1",
		headMemory:   "1Gi",
		headGPU:      "0",
		workerCPU:    "1",
		workerMemory: "1Gi",
		workerGPU:    "1",
	}

	t.Run("should error when the Ray cluster already exists", func(t *testing.T) {
		rayClusters := []runtime.Object{
			&rayv1.RayCluster{
				ObjectMeta: v1.ObjectMeta{
					Namespace: namespace,
					Name:      clusterName,
				},
				Spec: rayv1.RayClusterSpec{},
			},
		}

		rayClient := rayClientFake.NewSimpleClientset(rayClusters...)
		k8sClients := client.NewClientForTesting(kubefake.NewClientset(), rayClient)

		err := options.Run(context.Background(), k8sClients)
		require.Error(t, err)
	})
}

func TestNewCreateClusterCommand(t *testing.T) {
	testStreams, _, _, _ := genericclioptions.NewTestIOStreams()
	cmd := NewCreateClusterCommand(testStreams)

	t.Run("should have correct use and short description", func(t *testing.T) {
		assert.Equal(t, "cluster [CLUSTERNAME]", cmd.Use)
		assert.Equal(t, "Create Ray cluster", cmd.Short)
	})

	t.Run("should have all expected flags", func(t *testing.T) {
		flags := cmd.Flags()

		assert.NotNil(t, flags.Lookup("ray-version"))
		assert.NotNil(t, flags.Lookup("image"))
		assert.NotNil(t, flags.Lookup("head-cpu"))
		assert.NotNil(t, flags.Lookup("head-memory"))
		assert.NotNil(t, flags.Lookup("head-gpu"))
		assert.NotNil(t, flags.Lookup("head-ephemeral-storage"))
		assert.NotNil(t, flags.Lookup("head-ray-start-params"))
		assert.NotNil(t, flags.Lookup("worker-replicas"))
		assert.NotNil(t, flags.Lookup("worker-cpu"))
		assert.NotNil(t, flags.Lookup("worker-memory"))
		assert.NotNil(t, flags.Lookup("worker-gpu"))
		assert.NotNil(t, flags.Lookup("worker-tpu"))
		assert.NotNil(t, flags.Lookup("worker-ephemeral-storage"))
		assert.NotNil(t, flags.Lookup("worker-ray-start-params"))
		assert.NotNil(t, flags.Lookup("dry-run"))
		assert.NotNil(t, flags.Lookup("wait"))
		assert.NotNil(t, flags.Lookup("timeout"))
	})
}