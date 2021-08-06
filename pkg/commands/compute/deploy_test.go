package compute_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastly/cli/pkg/app"
	"github.com/fastly/cli/pkg/commands/compute/manifest"
	"github.com/fastly/cli/pkg/mock"
	"github.com/fastly/cli/pkg/testutil"
	"github.com/fastly/go-fastly/v3/fastly"
)

// NOTE: Some tests don't provide a Service ID via any mechanism (e.g. flag
// or manifest) and if one is provided the test will fail due to a specific
// API call not being mocked. Be careful not to add a Service ID to all tests
// without first checking whether the Service ID is expected as the user flow
// for when no Service ID is provided is to create a new service.
//
// Additionally, stdin can be mocked in one of two ways...
//
// 1. Provide a single value.
// 2. Provide multiple values (one for each prompt expected).
//
// In the first case, the first prompt given to the user will get the value you
// defined in the testcase.stdin field, all other prompts will get an empty
// value. This has worked fine for the most part as the prompts have
// historically provided default values when an empty value is encountered.
//
// The second case is to address running the test code successfully as the
// business logic has changed over time to now 'require' values to be provided
// for some prompts, this means an empty string will break the test flow. If
// that's what you're encountering, then you should add multiple values for the
// testcase.stdin field so that there is a value provided for every prompt your
// testcase user flow expects to encounter.
func TestDeploy(t *testing.T) {
	// We're going to chdir to a deploy environment,
	// so save the PWD to return to, afterwards.
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// Create test environment
	rootdir := testutil.NewEnv(testutil.EnvOpts{
		T: t,
		Copy: []testutil.FileIO{
			{
				Src: filepath.Join("testdata", "deploy", "pkg", "package.tar.gz"),
				Dst: filepath.Join("pkg", "package.tar.gz"),
			},
		},
	})
	defer os.RemoveAll(rootdir)

	// Before running the test, chdir into the build environment.
	// When we're done, chdir back to our original location.
	// This is so we can reliably copy the testdata/ fixtures.
	if err := os.Chdir(rootdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(pwd)

	args := testutil.Args
	for _, testcase := range []struct {
		api              mock.API
		args             []string
		dontWantOutput   []string
		manifest         string
		manifestIncludes string
		name             string
		noManifest       bool
		stdin            []string
		wantError        string
		wantOutput       []string
	}{
		{
			name:      "no token",
			args:      args("compute deploy"),
			wantError: "no token provided",
		},
		{
			name:       "no fastly.toml manifest",
			args:       args("compute deploy --token 123"),
			wantError:  "error reading package manifest",
			noManifest: true,
		},
		{
			// If no Service ID defined via flag or manifest, then the expectation is
			// for the service to be created via the API and for the returned ID to
			// be stored into the manifest.
			//
			// Additionally it validates that the specified path (files generated by
			// the testutil.NewEnv()) cause no issues.
			name: "path with no service ID",
			args: args("compute deploy --token 123 -v -p pkg/package.tar.gz"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			stdin: []string{"originless"},
			wantOutput: []string{
				"Setting service ID in manifest to \"12345\"...",
				"Deployed package (service 12345, version 1)",
			},
		},
		// Same validation as above with the exception that we use the default path
		// parsing logic (i.e. we don't explicitly pass a path via `-p` flag).
		{
			name: "empty service ID",
			args: args("compute deploy --token 123 -v"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			stdin: []string{"originless"},
			wantOutput: []string{
				"Setting service ID in manifest to \"12345\"...",
				"Deployed package (service 12345, version 1)",
			},
		},
		{
			name: "list versions error",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				GetServiceFn:   getServiceOK,
				ListVersionsFn: testutil.ListVersionsError,
			},
			wantError: fmt.Sprintf("error listing service versions: %s", testutil.Err.Error()),
		},
		{
			name: "service version is active, clone version error",
			args: args("compute deploy --service-id 123 --token 123 --version 1"),
			api: mock.API{
				ListVersionsFn: testutil.ListVersions,
				CloneVersionFn: testutil.CloneVersionError,
			},
			wantError: fmt.Sprintf("error cloning service version: %s", testutil.Err.Error()),
		},
		{
			name: "list domains error",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				GetServiceFn:   getServiceOK,
				ListVersionsFn: testutil.ListVersions,
				ListDomainsFn:  listDomainsError,
			},
			wantError: fmt.Sprintf("error fetching service domains: %s", testutil.Err.Error()),
		},
		{
			name: "list backends error",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				GetServiceFn:   getServiceOK,
				ListVersionsFn: testutil.ListVersions,
				ListDomainsFn:  listDomainsOk,
				ListBackendsFn: listBackendsError,
			},
			wantError: fmt.Sprintf("error fetching service backends: %s", testutil.Err.Error()),
		},
		// The following test doesn't just validate the package API error behaviour
		// but as a side effect it validates that when deleting the created
		// service, the Service ID is also cleared out from the manifest.
		{
			name: "package API error",
			args: args("compute deploy --token 123"),
			api: mock.API{
				CreateServiceFn: createServiceOK,
				CreateDomainFn:  createDomainOK,
				CreateBackendFn: createBackendOK,
				GetPackageFn:    getPackageOk,
				UpdatePackageFn: updatePackageError,
				DeleteBackendFn: deleteBackendOK,
				DeleteDomainFn:  deleteDomainOK,
				DeleteServiceFn: deleteServiceOK,
			},
			stdin:     []string{"originless"},
			wantError: fmt.Sprintf("error uploading package: %s", testutil.Err.Error()),
			wantOutput: []string{
				"Uploading package...",
			},
			manifestIncludes: `service_id = ""`,
		},
		// The following test doesn't provide a Service ID by either a flag nor the
		// manifest, so this will result in the deploy script attempting to create
		// a new service. We mock the API call to fail, and we expect to see a
		// relevant error message related to that error.
		{
			name: "service create error",
			args: args("compute deploy --token 123"),
			api: mock.API{
				CreateServiceFn: createServiceError,
			},
			stdin:     []string{"originless"},
			wantError: fmt.Sprintf("error creating service: %s", testutil.Err.Error()),
			wantOutput: []string{
				"Creating service...",
			},
		},
		// The following test doesn't provide a Service ID by either a flag nor the
		// manifest, so this will result in the deploy script attempting to create
		// a new service. We mock the service creation to be successful while we
		// mock the domain API call to fail, and we expect to see a relevant error
		// message related to that error.
		{
			name: "service domain error",
			args: args("compute deploy --token 123"),
			api: mock.API{
				CreateServiceFn: createServiceOK,
				CreateDomainFn:  createDomainError,
				DeleteDomainFn:  deleteDomainOK,
				DeleteServiceFn: deleteServiceOK,
			},
			stdin:     []string{"originless"},
			wantError: fmt.Sprintf("error creating domain: %s", testutil.Err.Error()),
			wantOutput: []string{
				"Creating service...",
				"Creating domain...",
			},
		},
		// The following test doesn't provide a Service ID by either a flag nor the
		// manifest, so this will result in the deploy script attempting to create
		// a new service. We mock the service creation to be successful while we
		// mock the backend API call to fail.
		{
			name: "service backend error",
			args: args("compute deploy --token 123"),
			api: mock.API{
				CreateServiceFn: createServiceOK,
				CreateDomainFn:  createDomainOK,
				CreateBackendFn: createBackendError,
				DeleteBackendFn: deleteBackendOK,
				DeleteDomainFn:  deleteDomainOK,
				DeleteServiceFn: deleteServiceOK,
			},
			stdin:     []string{"originless"},
			wantError: fmt.Sprintf("error creating backend: %s", testutil.Err.Error()),
			wantOutput: []string{
				"Creating service...",
				"Creating domain...",
				"Creating backend '127.0.0.1'...",
			},
		},
		// The following test validates that the undoStack is executed as expected
		// e.g. the backend and domain resources are deleted.
		{
			name: "activate error",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionError,
			},
			wantError: fmt.Sprintf("error activating version: %s", testutil.Err.Error()),
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
			},
		},
		{
			name: "identical package",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				ListVersionsFn: testutil.ListVersions,
				GetServiceFn:   getServiceOK,
				ListDomainsFn:  listDomainsOk,
				ListBackendsFn: listBackendsOk,
				GetPackageFn:   getPackageIdentical,
			},
			wantOutput: []string{
				"Skipping package deployment",
			},
		},
		{
			name: "success",
			args: args("compute deploy --service-id 123 --token 123"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Manage this service at:",
				"https://manage.fastly.com/configure/services/123",
				"View this service at:",
				"https://directly-careful-coyote.edgecompute.app",
				"Deployed package (service 123, version 3)",
			},
		},
		{
			name: "success with path",
			args: args("compute deploy --service-id 123 --token 123 -p pkg/package.tar.gz --version latest"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Manage this service at:",
				"https://manage.fastly.com/configure/services/123",
				"View this service at:",
				"https://directly-careful-coyote.edgecompute.app",
				"Deployed package (service 123, version 3)",
			},
		},
		{
			name: "success with inactive version",
			args: args("compute deploy --service-id 123 --token 123 -p pkg/package.tar.gz"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Deployed package (service 123, version 3)",
			},
		},
		{
			name: "success with specific locked version",
			args: args("compute deploy --service-id 123 --token 123 -p pkg/package.tar.gz --version 2"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				CloneVersionFn:    testutil.CloneVersionResult(4),
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Deployed package (service 123, version 4)",
			},
		},
		{
			name: "success with active version",
			args: args("compute deploy --service-id 123 --token 123 -p pkg/package.tar.gz --version active"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				CloneVersionFn:    testutil.CloneVersionResult(4),
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Deployed package (service 123, version 4)",
			},
		},
		{
			name: "success with comment",
			args: args("compute deploy --service-id 123 --token 123 -p pkg/package.tar.gz --version 2 --comment foo"),
			api: mock.API{
				GetServiceFn:      getServiceOK,
				ListVersionsFn:    testutil.ListVersions,
				CloneVersionFn:    testutil.CloneVersionResult(4),
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsOk,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				UpdateVersionFn:   updateVersionOk,
			},
			wantOutput: []string{
				"Uploading package...",
				"Activating version...",
				"Deployed package (service 123, version 4)",
			},
		},
		// The following test doesn't provide a Service ID by either a flag nor the
		// manifest, so this will result in the deploy script attempting to create
		// a new service. Our fastly.toml is configured with a [setup] section so
		// we expect to see the appropriate messaging in the output.
		//
		// It also validates the output displays the port number and backend name
		// when telling the user it is creating the backends. It only displays the
		// extra information when the --verbose flag is provided.
		{
			name: "success with setup configuration",
			args: args("compute deploy --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			manifest: `
			name = "package"
			manifest_version = 1
			language = "rust"

			[setup]
				[[setup.backends]]
					name = "backend_name"
					prompt = "Backend 1"
					address = "developer.fastly.com"
					port = 443
				[[setup.backends]]
					name = "other_backend_name"
					prompt = "Backend 2"
					address = "httpbin.org"
					port = 443
			`,
			wantOutput: []string{
				"Backend 1: [developer.fastly.com]",
				"Backend port number: [443]",
				"Backend 2: [httpbin.org]",
				"Backend port number: [443]",
				"Creating service...",
				"Creating domain...",
				"Creating backend 'developer.fastly.com' (port: 443, name: backend_name)...",
				"Creating backend 'httpbin.org' (port: 443, name: other_backend_name)...",
				"Uploading package...",
				"Activating version...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
		},
		// The following [setup] configuration doesn't define any prompts, nor any
		// ports, so we validate that the user prompts match our default expectations.
		{
			name: "success with setup configuration and no prompts or ports defined",
			args: args("compute deploy --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			manifest: `
			name = "package"
			manifest_version = 1
			language = "rust"

			[setup]
				[[setup.backends]]
					name = "foo_backend"
					address = "developer.fastly.com"
				[[setup.backends]]
					name = "bar_backend"
					address = "httpbin.org"
			`,
			wantOutput: []string{
				"Origin server for 'foo_backend': [developer.fastly.com]",
				"Backend port number: [80]",
				"Origin server for 'bar_backend': [httpbin.org]",
				"Backend port number: [80]",
				"Creating service...",
				"Creating domain...",
				"Creating backend 'developer.fastly.com' (port: 80, name: foo_backend)...",
				"Creating backend 'httpbin.org' (port: 80, name: bar_backend)...",
				"Uploading package...",
				"Activating version...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
		},
		// The following test validates no prompts are displayed to the user due to
		// the use of the --accept-defaults flag.
		{
			name: "success with setup configuration and accept-defaults",
			args: args("compute deploy --accept-defaults --token 123"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			manifest: `
			name = "package"
			manifest_version = 1
			language = "rust"

			[setup]
				[[setup.backends]]
					name = "backend_name"
					prompt = "Backend 1"
					address = "developer.fastly.com"
					port = 443
				[[setup.backends]]
					name = "other_backend_name"
					prompt = "Backend 2"
					address = "httpbin.org"
					port = 443
			`,
			wantOutput: []string{
				"Initializing...",
				"Creating service...",
				"Creating domain...",
				"Creating backend 'developer.fastly.com'...",
				"Creating backend 'httpbin.org'...",
				"Uploading package...",
				"Activating version...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
			dontWantOutput: []string{
				"Backend 1: [developer.fastly.com]",
				"Backend port number: [443]",
				"Backend 2: [httpbin.org]",
				"Backend port number: [443]",
			},
		},
		// The follow test validates the setup.backends.address field is a required
		// field. This is because we need an address to generate a name (if no name
		// was provided by the user).
		{
			name: "error with setup configuration and missing required fields",
			args: args("compute deploy --token 123"),
			manifest: `
			name = "package"
			manifest_version = 1
			language = "rust"

			[setup]
				[[setup.backends]]
					prompt = "Backend 1"
					port = 443
				[[setup.backends]]
					prompt = "Backend 2"
					port = 443
			`,
			wantError: "error parsing the [[setup.backends]] configuration",
		},
		// The following test validates the setup.backends.name field should be a
		// string, not an integer.
		{
			name: "error with setup configuration -- invalid setup.backends.name",
			args: args("compute deploy --token 123"),
			manifest: `
			name = "package"
			manifest_version = 1
			language = "rust"

			[setup]
				[[setup.backends]]
				  name = 123
					prompt = "Backend 1"
					address = "developer.fastly.com"
					port = 443
				[[setup.backends]]
				  name = 456
					prompt = "Backend 2"
					address = "httpbin.org"
					port = 443
			`,
			wantError: "error parsing the [[setup.backends]] configuration",
		},
		// The following test validates that a new 'originless' backend is created
		// when the user has no [setup] configuration and they also pass the
		// --accept-defaults flag.
		{
			name: "success with no setup configuration and --accept-defaults for new service creation",
			args: args("compute deploy --accept-defaults --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			wantOutput: []string{
				"Creating backend '127.0.0.1' (port: 80, name: originless)...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
		},
		{
			name: "success with no setup configuration and single backend entered at prompt for new service",
			args: args("compute deploy --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			stdin: []string{
				"fastly.com",
				"443",
				"my_backend_name",
				"", // this stops prompting for backends
				"", // this is to use the default domain
			},
			wantOutput: []string{
				"Backend (originless, hostname or IP address): [leave blank to stop adding backends]",
				"Backend port number: [80]",
				"Backend name:",
				"Creating backend 'fastly.com' (port: 443, name: my_backend_name)...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
		},
		// This is the same test as above but when prompted it will provide two
		// backends instead of one, and will also allow the code to generate the
		// backend name using its predefined formula.
		{
			name: "success with no setup configuration and multiple backends entered at prompt for new service",
			args: args("compute deploy --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			stdin: []string{
				"fastly.com",
				"443",
				"", // this is so we generate a backend name using a formula
				"google.com",
				"123",
				"", // this is so we generate a backend name using a formula
				"", // this stops prompting for backends
				"", // this is to use the default domain
			},
			wantOutput: []string{
				"Backend (originless, hostname or IP address): [leave blank to stop adding backends]",
				"Backend port number: [80]",
				"Backend name:",
				"Creating backend 'fastly.com' (port: 443, name: fastly_com)...",
				"Creating backend 'google.com' (port: 123, name: google_com)...",
				"SUCCESS: Deployed package (service 12345, version 1)",
			},
		},
		// The following test validates that when prompting the user for backends
		// that we must have an address defined and if they just press ENTER (i.e.
		// no value given) after the initial prompt then we'll error as we need at
		// least one backend address defined.
		{
			name: "error with no setup configuration and multiple backends prompted for new service",
			args: args("compute deploy --token 123 --verbose"),
			api: mock.API{
				CreateServiceFn:   createServiceOK,
				CreateDomainFn:    createDomainOK,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
				ListDomainsFn:     listDomainsOk,
			},
			wantError: "error configuring a backend (no input given)",
		},
		{
			name: "success with no setup configuration and multiple backends prompted for existing service with no backends",
			args: args("compute deploy --service-id 123 --token 123 --verbose"),
			api: mock.API{
				ListVersionsFn:    testutil.ListVersions,
				GetServiceFn:      getServiceOK,
				ListDomainsFn:     listDomainsOk,
				ListBackendsFn:    listBackendsNone,
				CreateBackendFn:   createBackendOK,
				GetPackageFn:      getPackageOk,
				UpdatePackageFn:   updatePackageOk,
				ActivateVersionFn: activateVersionOk,
			},
			stdin: []string{
				"fastly.com",
				"443",
				"", // this is so we generate a backend name using a formula
				"google.com",
				"123",
				"", // this is so we generate a backend name using a formula
				"", // this stops prompting for backends
			},
			wantOutput: []string{
				"Backend (originless, hostname or IP address): [leave blank to stop adding backends]",
				"Creating backend 'fastly.com' (port: 443, name: fastly_com)...",
				"Creating backend 'google.com' (port: 123, name: google_com)...",
				"SUCCESS: Deployed package (service 123, version 3)",
			},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			// Because the manifest can be mutated on each test scenario, we recreate
			// the file each time.
			manifestContent := `name = "package"`
			if testcase.manifest != "" {
				manifestContent = testcase.manifest
			}
			if err := os.WriteFile(filepath.Join(rootdir, manifest.Filename), []byte(manifestContent), 0777); err != nil {
				t.Fatal(err)
			}

			// For any test scenario that expects no manifest to exist, then instead
			// of deleting the manifest and having to recreate it, we'll simply
			// rename it, and then rename it back once the specific test scenario has
			// finished running.
			if testcase.noManifest {
				old := filepath.Join(rootdir, manifest.Filename)
				tmp := filepath.Join(rootdir, manifest.Filename+"Tmp")
				if err := os.Rename(old, tmp); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.Rename(tmp, old); err != nil {
						t.Fatal(err)
					}
				}()
			}

			var stdout bytes.Buffer
			opts := testutil.NewRunOpts(testcase.args, &stdout)
			opts.APIClient = mock.APIClient(testcase.api)

			if len(testcase.stdin) > 1 {
				// To handle multiple prompt input from the user we need to do some
				// coordination around io pipes to mimic the required user behaviour.
				stdin, prompt := io.Pipe()
				opts.Stdin = stdin

				// Wait for user input and write it to the prompt
				inputc := make(chan string)
				go func() {
					for input := range inputc {
						fmt.Fprintln(prompt, input)
					}
				}()

				// We need a channel so we wait for `run()` to complete
				done := make(chan bool)

				// Call `app.Run()` and wait for response
				go func() {
					err = app.Run(opts)
					done <- true
				}()

				// User provides input
				//
				// NOTE: Must provide as much input as is expected to be waited on by `run()`.
				//       For example, if `run()` calls `input()` twice, then provide two messages.
				//       Otherwise the select statement will trigger the timeout error.
				for _, input := range testcase.stdin {
					inputc <- input
				}

				select {
				case <-done:
					// Wait for app.Run() to finish
				case <-time.After(time.Second):
					t.Fatalf("unexpected timeout waiting for mocked prompt inputs to be processed")
				}
			} else {
				stdin := ""
				if len(testcase.stdin) > 0 {
					stdin = testcase.stdin[0]
				}
				opts.Stdin = strings.NewReader(stdin)
				err = app.Run(opts)
			}

			t.Log(stdout.String())

			testutil.AssertErrorContains(t, err, testcase.wantError)

			for _, s := range testcase.wantOutput {
				testutil.AssertStringContains(t, stdout.String(), s)
			}

			for _, s := range testcase.dontWantOutput {
				testutil.AssertStringDoesntContain(t, stdout.String(), s)
			}

			if testcase.manifestIncludes != "" {
				content, err := os.ReadFile(filepath.Join(rootdir, manifest.Filename))
				if err != nil {
					t.Fatal(err)
				}
				testutil.AssertStringContains(t, string(content), testcase.manifestIncludes)
			}
		})
	}
}

func createServiceOK(i *fastly.CreateServiceInput) (*fastly.Service, error) {
	return &fastly.Service{
		ID:   "12345",
		Name: i.Name,
		Type: i.Type,
	}, nil
}

func createServiceError(*fastly.CreateServiceInput) (*fastly.Service, error) {
	return nil, testutil.Err
}

func deleteServiceOK(i *fastly.DeleteServiceInput) error {
	return nil
}

func createDomainError(i *fastly.CreateDomainInput) (*fastly.Domain, error) {
	return nil, testutil.Err
}

func deleteDomainOK(i *fastly.DeleteDomainInput) error {
	return nil
}

func createBackendError(i *fastly.CreateBackendInput) (*fastly.Backend, error) {
	return nil, testutil.Err
}

func deleteBackendOK(i *fastly.DeleteBackendInput) error {
	return nil
}

func getPackageIdentical(i *fastly.GetPackageInput) (*fastly.Package, error) {
	return &fastly.Package{
		ServiceID:      i.ServiceID,
		ServiceVersion: i.ServiceVersion,
		Metadata: fastly.PackageMetadata{
			HashSum: "2b742f99854df7e024c287e36fb0fdfc5414942e012be717e52148ea0d6800d66fc659563f6f11105815051e82b14b61edc84b33b49789b790db1ed3446fb483",
		},
	}, nil
}

func activateVersionError(i *fastly.ActivateVersionInput) (*fastly.Version, error) {
	return nil, testutil.Err
}

func listDomainsError(i *fastly.ListDomainsInput) ([]*fastly.Domain, error) {
	return nil, testutil.Err
}

func listBackendsError(i *fastly.ListBackendsInput) ([]*fastly.Backend, error) {
	return nil, testutil.Err
}
