package test

//	MIT License
//
//	Copyright (c) Microsoft Corporation. All rights reserved.
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE

import (
	"context"
	"flag"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-amqp-common-go"
	mgmt "github.com/Azure/azure-sdk-for-go/services/eventhub/mgmt/2017-04-01/eventhub"
	rm "github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2017-05-10/resources"
	"github.com/Azure/go-autorest/autorest/azure"
	azauth "github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/suite"
	"github.com/uber/jaeger-client-go"
	"github.com/uber/jaeger-client-go/config"
	jaegerlog "github.com/uber/jaeger-client-go/log"
)

var (
	letterRunes = []rune("abcdefghijklmnopqrstuvwxyz123456789")
	debug       = flag.Bool("debug", false, "output debug level logging")
)

const (
	defaultTimeout = 1 * time.Minute
)

const (
	// Location is the Azure geographic location the test suite will use for provisioning
	Location = "eastus"

	// ResourceGroupName is the name of the resource group the test suite will use for provisioning
	ResourceGroupName = "ehtest"
)

type (
	// BaseSuite encapsulates a end to end test of Event Hubs with build up and tear down of all EH resources
	BaseSuite struct {
		suite.Suite
		SubscriptionID string
		Namespace      string
		Env            azure.Environment
		TagID          string
		closer         io.Closer
	}

	// HubMgmtOption represents an option for configuring an Event Hub.
	HubMgmtOption func(model *mgmt.Model) error
	// NamespaceMgmtOption represents an option for configuring a Namespace
	NamespaceMgmtOption func(ns *mgmt.EHNamespace) error
)

func init() {
	rand.Seed(time.Now().Unix())
}

// SetupSuite constructs the test suite from the environment and
func (suite *BaseSuite) SetupSuite() {
	flag.Parse()
	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	suite.SubscriptionID = mustGetEnv("AZURE_SUBSCRIPTION_ID")
	suite.Namespace = mustGetEnv("EVENTHUB_NAMESPACE")
	envName := os.Getenv("AZURE_ENVIRONMENT")
	suite.TagID = RandomString("tag", 5)

	if envName == "" {
		suite.Env = azure.PublicCloud
	} else {
		var err error
		env, err := azure.EnvironmentFromName(envName)
		if !suite.NoError(err) {
			suite.FailNow("could not find env name")
		}
		suite.Env = env
	}

	if !suite.NoError(suite.ensureProvisioned(mgmt.SkuTierStandard)) {
		suite.FailNow("failed provisioning")
	}

	if !suite.NoError(suite.setupTracing()) {
		suite.FailNow("failed to setup tracing")
	}
}

// TearDownSuite might one day destroy all of the resources in the suite, but I'm not sure we want to do that just yet...
func (suite *BaseSuite) TearDownSuite() {
	// maybe tear down all existing resource??
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	suite.deleteAllTaggedEventHubs(ctx)
	if suite.closer != nil {
		suite.closer.Close()
	}
}

// RandomHub creates a hub with a random'ish name
func (suite *BaseSuite) RandomHub(opts ...HubMgmtOption) (*mgmt.Model, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout*2)
	defer cancel()

	name := suite.RandomName("goehtest", 6)
	model, err := suite.ensureEventHub(ctx, name, opts...)
	suite.Require().NoError(err)
	suite.Require().NotNil(model.PartitionIds)
	suite.Require().Len(*model.PartitionIds, 4)
	return model, func() {
		if model != nil {
			err := suite.DeleteEventHub(*model.Name)
			if err != nil {
				suite.T().Log(err)
			}
		}
	}
}

// EnsureEventHub creates an Event Hub if it doesn't exist
func (suite *BaseSuite) ensureEventHub(ctx context.Context, name string, opts ...HubMgmtOption) (*mgmt.Model, error) {
	client := suite.getEventHubMgmtClient()
	hub, err := client.Get(ctx, ResourceGroupName, suite.Namespace, name)

	if err != nil {
		newHub := &mgmt.Model{
			Name: &name,
			Properties: &mgmt.Properties{
				PartitionCount: common.PtrInt64(4),
			},
		}

		for _, opt := range opts {
			err = opt(newHub)
			if err != nil {
				return nil, err
			}
		}

		var lastErr error
		deadline, _ := ctx.Deadline()
		for time.Now().Before(deadline) {
			hub, err = suite.tryHubCreate(ctx, client, name, newHub)
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
		}

		if lastErr != nil {
			return nil, lastErr
		}
	}
	return &hub, nil
}

func (suite *BaseSuite) tryHubCreate(ctx context.Context, client *mgmt.EventHubsClient, name string, hub *mgmt.Model) (mgmt.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	//suite.T().Logf("trying to create hub named %q", name)
	createdHub, err := client.CreateOrUpdate(ctx, ResourceGroupName, suite.Namespace, name, *hub)
	if err != nil {
		//suite.T().Logf("failed to create hub named %q", name)
		return mgmt.Model{}, err
	}

	return createdHub, err
}

// DeleteEventHub deletes an Event Hub within the given Namespace
func (suite *BaseSuite) DeleteEventHub(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	client := suite.getEventHubMgmtClient()
	_, err := client.Delete(ctx, ResourceGroupName, suite.Namespace, name)
	return err
}

func (suite *BaseSuite) deleteAllTaggedEventHubs(ctx context.Context) {
	client := suite.getEventHubMgmtClient()
	res, err := client.ListByNamespace(ctx, ResourceGroupName, suite.Namespace, to.Int32Ptr(0), to.Int32Ptr(20))
	if err != nil {
		suite.T().Log("error listing namespaces")
		suite.T().Error(err)
	}

	for res.NotDone() {
		for _, val := range res.Values() {
			if strings.Contains(*val.Name, suite.TagID) {
				for i := 0; i < 5; i++ {
					if _, err := client.Delete(ctx, ResourceGroupName, suite.Namespace, *val.Name); err != nil {
						suite.T().Logf("error deleting %q", *val.Name)
						suite.T().Error(err)
						time.Sleep(3 * time.Second)
					} else {
						break
					}
				}
			} else {
				suite.T().Logf("%q does not contain %q", *val.Name, suite.TagID)
			}
		}
		res.Next()
	}
}

func (suite *BaseSuite) ensureProvisioned(tier mgmt.SkuTier) error {
	_, err := ensureResourceGroup(context.Background(), suite.SubscriptionID, ResourceGroupName, Location, suite.Env)
	if err != nil {
		return err
	}

	_, err = suite.ensureNamespace()
	return err
}

// ensureResourceGroup creates a Azure Resource Group if it does not already exist
func ensureResourceGroup(ctx context.Context, subscriptionID, name, location string, env azure.Environment) (*rm.Group, error) {
	groupClient := getRmGroupClientWithToken(subscriptionID, env)
	group, err := groupClient.Get(ctx, name)
	if group.Response.Response == nil {
		// tcp dial error or something else where the response was not populated
		return nil, err
	}

	if group.StatusCode == http.StatusNotFound {
		group, err = groupClient.CreateOrUpdate(ctx, name, rm.Group{Location: common.PtrString(location)})
		if err != nil {
			return nil, err
		}
	} else if group.StatusCode >= 400 {
		return nil, err
	}

	return &group, nil
}

// ensureNamespace creates a Azure Event Hub Namespace if it does not already exist
func ensureNamespace(ctx context.Context, subscriptionID, rg, name, location string, env azure.Environment, opts ...NamespaceMgmtOption) (*mgmt.EHNamespace, error) {
	_, err := ensureResourceGroup(ctx, subscriptionID, rg, location, env)
	if err != nil {
		return nil, err
	}

	client := getNamespaceMgmtClientWithToken(subscriptionID, env)
	namespace, err := client.Get(ctx, rg, name)
	if err != nil {
		return nil, err
	}

	if namespace.StatusCode == 404 {
		newNamespace := &mgmt.EHNamespace{
			Name: &name,

			Sku: &mgmt.Sku{
				Name:     mgmt.Basic,
				Tier:     mgmt.SkuTierBasic,
				Capacity: common.PtrInt32(1),
			},
			EHNamespaceProperties: &mgmt.EHNamespaceProperties{
				IsAutoInflateEnabled:   common.PtrBool(false),
				MaximumThroughputUnits: common.PtrInt32(1),
			},
		}

		for _, opt := range opts {
			err = opt(newNamespace)
			if err != nil {
				return nil, err
			}
		}

		nsFuture, err := client.CreateOrUpdate(ctx, rg, name, *newNamespace)
		if err != nil {
			return nil, err
		}

		namespace, err = nsFuture.Result(*client)
		if err != nil {
			return nil, err
		}
	} else if namespace.StatusCode >= 400 {
		return nil, err
	}

	return &namespace, nil
}

func (suite *BaseSuite) getEventHubMgmtClient() *mgmt.EventHubsClient {
	client := mgmt.NewEventHubsClientWithBaseURI(suite.Env.ResourceManagerEndpoint, suite.SubscriptionID)
	a, err := azauth.NewAuthorizerFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	client.Authorizer = a
	return &client
}

func (suite *BaseSuite) ensureNamespace() (*mgmt.EHNamespace, error) {
	ns, err := ensureNamespace(context.Background(), suite.SubscriptionID, ResourceGroupName, suite.Namespace, Location, suite.Env)
	if err != nil {
		return nil, err
	}
	return ns, err
}

func (suite *BaseSuite) setupTracing() error {
	if os.Getenv("TRACING") == "true" {
		// Sample configuration for testing. Use constant sampling to sample every trace
		// and enable LogSpan to log every span via configured Logger.
		cfg := config.Configuration{
			Sampler: &config.SamplerConfig{
				Type:  jaeger.SamplerTypeConst,
				Param: 1,
			},
			Reporter: &config.ReporterConfig{
				LocalAgentHostPort: "0.0.0.0:6831",
			},
		}

		// Example logger and metrics factory. Use github.com/uber/jaeger-client-go/log
		// and github.com/uber/jaeger-lib/metrics respectively to bind to real logging and metrics
		// frameworks.
		jLogger := jaegerlog.StdLogger

		closer, err := cfg.InitGlobalTracer(
			"ehtests",
			config.Logger(jLogger),
		)
		if !suite.NoError(err) {
			suite.FailNow("failed to initialize the global trace logger")
		}

		suite.closer = closer
		return err
	}
	return nil
}

func getNamespaceMgmtClientWithToken(subscriptionID string, env azure.Environment) *mgmt.NamespacesClient {
	client := mgmt.NewNamespacesClientWithBaseURI(env.ResourceManagerEndpoint, subscriptionID)
	a, err := azauth.NewAuthorizerFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	client.Authorizer = a
	return &client
}

func getRmGroupClientWithToken(subscriptionID string, env azure.Environment) *rm.GroupsClient {
	groupsClient := rm.NewGroupsClientWithBaseURI(env.ResourceManagerEndpoint, subscriptionID)
	a, err := azauth.NewAuthorizerFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	groupsClient.Authorizer = a
	return &groupsClient
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("Env variable '" + key + "' required for integration tests.")
	}
	return v
}

// RandomName generates a random Event Hub name tagged with the suite id
func (suite *BaseSuite) RandomName(prefix string, length int) string {
	return RandomString(prefix, length) + "-" + suite.TagID
}

// RandomString generates a random string with prefix
func RandomString(prefix string, length int) string {
	b := make([]rune, length)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return prefix + string(b)
}
