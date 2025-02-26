package mage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dagger.io/dagger"
	"github.com/dagger/dagger/internal/mage/sdk"
	"github.com/dagger/dagger/internal/mage/util"
	"github.com/magefile/mage/mg" // mg contains helpful utility functions, like Deps
	"github.com/moby/buildkit/identity"
	"golang.org/x/mod/semver"
)

var publishedEngineArches = []string{"amd64", "arm64"}

func parseRef(tag string) error {
	if tag == "main" {
		return nil
	}
	if ok := semver.IsValid(tag); !ok {
		return fmt.Errorf("invalid semver tag: %s", tag)
	}
	return nil
}

type Engine mg.Namespace

// Build builds the dagger cli binary
func (t Engine) Build(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()
	c = c.Pipeline("engine").Pipeline("build")

	_, err = util.HostDaggerBinary(c).Export(ctx, "./bin/dagger")

	return err
}

// Lint lints the engine
func (t Engine) Lint(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("engine").Pipeline("lint")

	repo := util.RepositoryGoCodeOnly(c)

	_, err = c.Container().
		From("golangci/golangci-lint:v1.51-alpine").
		WithMountedDirectory("/app", repo).
		WithWorkdir("/app").
		WithExec([]string{"golangci-lint", "run", "-v", "--timeout", "5m"}).
		Sync(ctx)
	return err
}

// Publish builds and pushes Engine OCI image to a container registry
func (t Engine) Publish(ctx context.Context, version string) error {
	if err := parseRef(version); err != nil {
		return err
	}

	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("engine").Pipeline("publish")

	var (
		engineImage = util.GetHostEnv("DAGGER_ENGINE_IMAGE")
		ref         = fmt.Sprintf("%s:%s", engineImage, version)
	)

	digest, err := c.Container().Publish(ctx, ref, dagger.ContainerPublishOpts{
		PlatformVariants: util.DevEngineContainer(c, publishedEngineArches, version),
	})
	if err != nil {
		return err
	}

	if semver.IsValid(version) {
		sdks := sdk.All{}
		if err := sdks.Bump(ctx, version); err != nil {
			return err
		}
	} else {
		fmt.Printf("'%s' is not a semver version, skipping image bump in SDKs", version)
	}

	time.Sleep(3 * time.Second) // allow buildkit logs to flush, to minimize potential confusion with interleaving
	fmt.Println("PUBLISHED IMAGE REF:", digest)

	return nil
}

// Verify that all arches for the engine can be built. Just do a local export to avoid setting up
// a registry
func (t Engine) TestPublish(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("engine").Pipeline("test-publish")
	_, err = c.Container().Export(ctx, "./engine.tar", dagger.ContainerExportOpts{
		PlatformVariants: util.DevEngineContainer(c, publishedEngineArches, ""),
	})
	return err
}

func registry(c *dagger.Client) *dagger.Container {
	return c.Pipeline("registry").Container().From("registry:2").
		WithExposedPort(5000, dagger.ContainerWithExposedPortOpts{Protocol: dagger.Tcp}).
		WithExec(nil)
}

func privateRegistry(c *dagger.Client) *dagger.Container {
	const htpasswd = "john:$2y$05$/iP8ud0Fs8o3NLlElyfVVOp6LesJl3oRLYoc3neArZKWX10OhynSC" //nolint:gosec
	return c.Pipeline("private registry").Container().From("registry:2").
		WithNewFile("/auth/htpasswd", dagger.ContainerWithNewFileOpts{Contents: htpasswd}).
		WithEnvVariable("REGISTRY_AUTH", "htpasswd").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_REALM", "Registry Realm").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_PATH", "/auth/htpasswd").
		WithExposedPort(5000, dagger.ContainerWithExposedPortOpts{Protocol: dagger.Tcp}).
		WithExec(nil)
}

func (t Engine) test(ctx context.Context, race bool) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("engine").Pipeline("test")

	opts := util.DevEngineOpts{
		ConfigEntries: map[string]string{
			`registry."registry:5000"`:        "http = true",
			`registry."privateregistry:5000"`: "http = true",
		},
	}
	devEngine := util.DevEngineContainer(c.Pipeline("dev-engine"), []string{runtime.GOARCH}, "", util.DefaultDevEngineOpts, opts)[0]

	// This creates an engine.tar container file that can be used by the integration tests.
	// In particular, it is used by core/integration/remotecache_test.go to create a
	// dev engine that can be used to test remote caching.
	// I also load the dagger binary, so that the remote cache tests can use it to
	// run dagger queries.

	tmpDir, err := os.MkdirTemp("", "dagger-dev-engine-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	_, err = devEngine.Export(ctx, path.Join(tmpDir, "engine.tar"))
	if err != nil {
		return err
	}

	// These are used by core/integration/remotecache_test.go
	testEngineUtils := c.Host().Directory(tmpDir, dagger.HostDirectoryOpts{
		Include: []string{"engine.tar"},
	}).WithFile("/dagger", util.DaggerBinary(c), dagger.DirectoryWithFileOpts{
		Permissions: 0755,
	})

	registrySvc := registry(c)
	devEngine = devEngine.
		WithServiceBinding("registry", registrySvc).
		WithServiceBinding("privateregistry", privateRegistry(c)).
		WithExposedPort(1234, dagger.ContainerWithExposedPortOpts{Protocol: dagger.Tcp}).
		WithMountedCache("/var/lib/dagger", c.CacheVolume("dagger-dev-engine-test-state"+identity.NewID())).
		WithExec(nil, dagger.ContainerWithExecOpts{
			InsecureRootCapabilities: true,
		})

	endpoint, err := devEngine.Endpoint(ctx, dagger.ContainerEndpointOpts{Port: 1234, Scheme: "tcp"})
	if err != nil {
		return err
	}

	cgoEnabledEnv := "0"
	args := []string{
		"gotestsum",
		"--format", "testname",
		"--no-color=false",
		"--jsonfile=./tests.log",
		"--",
		// go test flags
		"-parallel=16",
		"-count=1",
		"-timeout=15m",
	}

	if race {
		args = append(args, "-race", "-timeout=1h")
		cgoEnabledEnv = "1"
	}

	args = append(args, "./...")
	cliBinPath := "/.dagger-cli"

	utilDirPath := "/dagger-dev"
	tests := util.GoBase(c).
		WithExec([]string{"go", "install", "gotest.tools/gotestsum@v1.10.0"}).
		WithMountedDirectory("/app", util.Repository(c)). // need all the source for extension tests
		WithMountedDirectory(utilDirPath, testEngineUtils).
		WithEnvVariable("_DAGGER_TESTS_ENGINE_TAR", filepath.Join(utilDirPath, "engine.tar")).
		WithWorkdir("/app").
		WithServiceBinding("dagger-engine", devEngine).
		WithServiceBinding("registry", registrySvc).
		WithEnvVariable("CGO_ENABLED", cgoEnabledEnv)

	// TODO use Container.With() to set this. It'll be much nicer.
	cacheEnv, set := os.LookupEnv("_EXPERIMENTAL_DAGGER_CACHE_CONFIG")
	if set {
		tests = tests.WithEnvVariable("_EXPERIMENTAL_DAGGER_CACHE_CONFIG", cacheEnv)
	}

	_, err = tests.
		WithMountedFile(cliBinPath, util.DaggerBinary(c)).
		WithEnvVariable("_EXPERIMENTAL_DAGGER_CLI_BIN", cliBinPath).
		WithEnvVariable("_EXPERIMENTAL_DAGGER_RUNNER_HOST", endpoint).
		WithMountedDirectory("/root/.docker", util.HostDockerDir(c)).
		WithExec(args).
		WithExec([]string{"gotestsum", "tool", "slowest", "--jsonfile=./tests.log", "--threshold=1s"}).
		Sync(ctx)
	return err
}

// Test runs Engine tests
func (t Engine) Test(ctx context.Context) error {
	return t.test(ctx, false)
}

// TestRace runs Engine tests with go race detector enabled
func (t Engine) TestRace(ctx context.Context) error {
	return t.test(ctx, true)
}

func (t Engine) Dev(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("engine").Pipeline("dev")

	arches := []string{runtime.GOARCH}

	tarPath := "./bin/engine.tar"

	_, err = c.Container().Export(ctx, tarPath, dagger.ContainerExportOpts{
		PlatformVariants: util.DevEngineContainer(c, arches, ""),
	})
	if err != nil {
		return err
	}

	volumeName := util.EngineContainerName
	imageName := fmt.Sprintf("localhost/%s:latest", util.EngineContainerName)

	// #nosec
	loadCmd := exec.CommandContext(ctx, "docker", "load", "-i", tarPath)
	output, err := loadCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker load failed: %w: %s", err, output)
	}
	_, imageID, ok := strings.Cut(string(output), "sha256:")
	if !ok {
		return fmt.Errorf("unexpected output from docker load: %s", output)
	}
	imageID = strings.TrimSpace(imageID)

	if output, err := exec.CommandContext(ctx, "docker",
		"tag",
		imageID,
		imageName,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("docker tag: %w: %s", err, output)
	}

	if output, err := exec.CommandContext(ctx, "docker",
		"rm",
		"-fv",
		util.EngineContainerName,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, output)
	}

	runArgs := []string{
		"run",
		"-d",
		// "--rm",
		"-e", util.CacheConfigEnvName,
		"-e", util.ServicesDNSEnvName,
		"-e", "_EXPERIMENTAL_DAGGER_CLOUD_TOKEN",
		"-e", "_EXPERIMENTAL_DAGGER_CLOUD_URL",
		"-v", volumeName + ":" + util.EngineDefaultStateDir,
		"--name", util.EngineContainerName,
		"--privileged",
	}
	runArgs = append(runArgs, imageName, "--debug")

	if output, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %w: %s", err, output)
	}

	// build the CLI and export locally so it can be used to connect to the engine
	binDest := filepath.Join(os.Getenv("DAGGER_SRC_ROOT"), "bin", "dagger")
	_, err = util.HostDaggerBinary(c).Export(ctx, binDest)
	if err != nil {
		return err
	}

	fmt.Println("export _EXPERIMENTAL_DAGGER_CLI_BIN=" + binDest)
	fmt.Println("export _EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://" + util.EngineContainerName)
	return nil
}
