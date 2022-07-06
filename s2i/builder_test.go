package s2i_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"github.com/openshift/source-to-image/pkg/api"
	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/s2i"
	. "knative.dev/kn-plugin-func/testing"
)

// Test_ErrRuntimeRequired ensures that a request to build without a runtime
// defined for the Function yields an ErrRuntimeRequired
func Test_ErrRuntimeRequired(t *testing.T) {
	b := s2i.NewBuilder()
	err := b.Build(context.Background(), fn.Function{})

	if !errors.Is(err, s2i.ErrRuntimeRequired) {
		t.Fatal("expected ErrRuntimeRequired not received")
	}
}

// Test_ErrRuntimeNotSupported ensures that a request to build a function whose
// runtime is not yet supported yields an ErrRuntimeNotSupported
func Test_ErrRuntimeNotSupported(t *testing.T) {
	b := s2i.NewBuilder()
	err := b.Build(context.Background(), fn.Function{Runtime: "unsupported"})

	if !errors.Is(err, s2i.ErrRuntimeNotSupported) {
		t.Fatal("expected ErrRuntimeNotSupported not received")
	}
}

// Test_BuilderImageDefault ensures that a Function being built which does not
// define a Builder Image will default.
func Test_ImageDefault(t *testing.T) {
	var (
		i = &mockImpl{}                                              // mock underlying s2i implementation
		c = mockDocker{}                                             // mock docker client
		b = s2i.NewBuilder(s2i.WithImpl(i), s2i.WithDockerClient(c)) // Func S2I Builder logic
		f = fn.Function{Runtime: "node"}                             // Function with no builder image set
	)

	// An implementation of the underlying S2I implementation which verifies
	// the config has arrived as expected (correct Functions logic applied)
	i.BuildFn = func(cfg *api.Config) (*api.Result, error) {
		expected := s2i.DefaultBuilderImages["node"]
		if cfg.BuilderImage != expected {
			t.Fatalf("expected s2i config builder image '%v', got '%v'", expected, cfg.BuilderImage)
		}
		return nil, nil
	}

	// Invoke Build, which runs Function Builder logic before invoking the
	// mock impl above.
	if err := b.Build(context.Background(), f); err != nil {
		t.Fatal(err)
	}
}

// Test_BuilderImageConfigurable ensures that the builder will use the builder
// image defined on the given Function if provided.
func Test_BuilderImageConfigurable(t *testing.T) {
	var (
		i = &mockImpl{}                                              // mock underlying s2i implementation
		c = mockDocker{}                                             // mock docker client
		b = s2i.NewBuilder(s2i.WithImpl(i), s2i.WithDockerClient(c)) // Func S2I Builder logic
		f = fn.Function{                                             // Function with a builder image set
			Runtime: "node",
			Build: fn.BuildSpec{BuilderImages: map[string]string{
				"s2i": "example.com/user/builder-image",
			}},
		}
	)

	// An implementation of the underlying S2I implementation which verifies
	// the config has arrived as expected (correct Functions logic applied)
	i.BuildFn = func(cfg *api.Config) (*api.Result, error) {
		expected := f.Build.BuilderImages["s2i"]
		if cfg.BuilderImage != expected {
			t.Fatalf("expected s2i config builder image for node to be '%v', got '%v'", expected, cfg.BuilderImage)
		}
		return nil, nil
	}

	// Invoke Build, which runs Function Builder logic before invoking the
	// mock impl above.
	if err := b.Build(context.Background(), f); err != nil {
		t.Fatal(err)
	}
}

// Test_Verbose ensures that the verbosity flag is propagated to the
// S2I builder implementation.
func Test_BuilderVerbose(t *testing.T) {
	c := mockDocker{} // mock docker client
	assert := func(verbose bool) {
		i := &mockImpl{
			BuildFn: func(cfg *api.Config) (r *api.Result, err error) {
				if cfg.Quiet == verbose {
					t.Fatalf("expected s2i quiet mode to be !%v when verbose %v", verbose, verbose)
				}
				return &api.Result{Messages: []string{"message"}}, nil
			}}
		if err := s2i.NewBuilder(s2i.WithVerbose(verbose), s2i.WithImpl(i), s2i.WithDockerClient(c)).Build(context.Background(), fn.Function{Runtime: "node"}); err != nil {
			t.Fatal(err)
		}
	}
	assert(true)  // when verbose is on, quiet should remain off
	assert(false) // when verbose is off, quiet should be toggled on
}

// Test_BuildEnvs ensures that build environment variables on the Function
// are interpolated and passed to the S2I build implementation in the final
// build config.
func Test_BuildEnvs(t *testing.T) {
	defer WithEnvVar(t, "INTERPOLATE_ME", "interpolated")()
	var (
		envName  = "NAME"
		envValue = "{{ env:INTERPOLATE_ME }}"
		f        = fn.Function{
			Runtime: "node",
			Build:   fn.BuildSpec{BuildEnvs: []fn.Env{{Name: &envName, Value: &envValue}}},
		}
		i = &mockImpl{}
		c = mockDocker{}
		b = s2i.NewBuilder(s2i.WithImpl(i), s2i.WithDockerClient(c))
	)
	i.BuildFn = func(cfg *api.Config) (r *api.Result, err error) {
		for _, v := range cfg.Environment {
			if v.Name == envName && v.Value == "interpolated" {
				return // success!
			} else if v.Name == envName && v.Value == envValue {
				t.Fatal("build env was not interpolated")
			}
		}
		t.Fatal("build envs not added to builder impl config")
		return
	}
	if err := b.Build(context.Background(), f); err != nil {
		t.Fatal(err)
	}
}

func TestS2IScriptURL(t *testing.T) {
	testRegistry := startRegistry(t)

	// builder that is only in registry not in daemon
	remoteBuilder := testRegistry + "/default/builder:remote"
	// builder that is in daemon
	localBuilder := "example.com/default/builder:local"

	// begin push testing builder to registry
	tag, err := name.NewTag(remoteBuilder)
	if err != nil {
		t.Fatal(err)
	}

	img, err := tarball.ImageFromPath(filepath.Join("testData", "builder.tar"), nil)
	if err != nil {
		t.Fatal(err)
	}

	err = remote.Write(&tag, img)
	if err != nil {
		t.Fatal(err)
	}
	// end push testing builder to registry

	scriptURL := "image:///usr/local/s2i"
	cli := mockDocker{
		inspect: func(ctx context.Context, image string) (types.ImageInspect, []byte, error) {
			if image != localBuilder {
				return types.ImageInspect{}, nil, notFoundErr{}
			}
			return types.ImageInspect{
				Config: &container.Config{Labels: map[string]string{"io.openshift.s2i.scripts-url": scriptURL}},
			}, nil, nil
		},
	}
	impl := &mockImpl{
		BuildFn: func(config *api.Config) (*api.Result, error) {
			if config.ScriptsURL != scriptURL {
				return nil, fmt.Errorf("unexepeted ScriptURL: %q", config.ScriptsURL)
			}
			return nil, nil
		},
	}

	tests := []struct {
		name         string
		builderImage string
	}{
		{name: "builder in daemon", builderImage: localBuilder},
		{name: "builder not in daemon", builderImage: remoteBuilder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fn.Function{
				Runtime: "node",
				Build: fn.BuildSpec{BuilderImages: map[string]string{
					"s2i": tt.builderImage,
				}},
			}

			b := s2i.NewBuilder(s2i.WithImpl(impl), s2i.WithDockerClient(cli))
			err = b.Build(context.Background(), f)
			if err != nil {
				t.Error(err)
			}
		})
	}

}

func startRegistry(t *testing.T) (addr string) {
	s := http.Server{
		Handler: registry.New(registry.Logger(log.New(io.Discard, "", 0))),
	}
	t.Cleanup(func() { s.Close() })

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()

	go func() {
		err = s.Serve(l)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintln(os.Stderr, "ERROR: ", err)
		}
	}()

	return addr
}

func TestBuildContextUpload(t *testing.T) {

	dockerfileContent := []byte("FROM scratch\nLABEL A=42")
	atxtContent := []byte("hello world!\n")

	cli := mockDocker{
		build: func(ctx context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
			tr := tar.NewReader(context)
			for {
				hdr, err := tr.Next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return types.ImageBuildResponse{}, err
				}
				switch hdr.Name {
				case ".":
				case "Dockerfile":
					bs, err := ioutil.ReadAll(tr)
					if err != nil {
						return types.ImageBuildResponse{}, err
					}
					if !bytes.Equal(bs, dockerfileContent) {
						return types.ImageBuildResponse{}, errors.New("bad content for Dockerfile")
					}
				case "a.txt":
					bs, err := ioutil.ReadAll(tr)
					if err != nil {
						return types.ImageBuildResponse{}, err
					}
					if !bytes.Equal(bs, atxtContent) {
						return types.ImageBuildResponse{}, errors.New("bad content for a.txt")
					}
				default:
					return types.ImageBuildResponse{}, errors.New("unexpected file")
				}
			}
			return types.ImageBuildResponse{
				Body:   io.NopCloser(strings.NewReader(`{"stream": "OK!"}`)),
				OSType: "linux",
			}, nil
		},
	}

	impl := &mockImpl{
		BuildFn: func(config *api.Config) (*api.Result, error) {
			err := ioutil.WriteFile(config.AsDockerfile, dockerfileContent, 0644)
			if err != nil {
				return nil, err
			}
			err = ioutil.WriteFile(filepath.Join(filepath.Dir(config.AsDockerfile), "a.txt"), atxtContent, 0644)
			if err != nil {
				return nil, err
			}

			return nil, nil
		},
	}

	f := fn.Function{
		Runtime: "node",
	}
	b := s2i.NewBuilder(s2i.WithImpl(impl), s2i.WithDockerClient(cli))
	err := b.Build(context.Background(), f)
	if err != nil {
		t.Error(err)
	}
}

func TestBuildFail(t *testing.T) {
	cli := mockDocker{
		build: func(ctx context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
			return types.ImageBuildResponse{
				Body:   io.NopCloser(strings.NewReader(`{"errorDetail": {"message": "Error: this is expected"}}`)),
				OSType: "linux",
			}, nil
		},
	}
	impl := &mockImpl{
		BuildFn: func(config *api.Config) (*api.Result, error) {
			return &api.Result{Success: true}, nil
		},
	}
	b := s2i.NewBuilder(s2i.WithImpl(impl), s2i.WithDockerClient(cli))
	err := b.Build(context.Background(), fn.Function{Runtime: "node"})
	if err == nil || !strings.Contains(err.Error(), "Error: this is expected") {
		t.Error("didn't get expected error")
	}
}

// mockImpl is a mock implementation of an S2I builder.
type mockImpl struct {
	BuildFn func(*api.Config) (*api.Result, error)
}

func (i *mockImpl) Build(cfg *api.Config) (*api.Result, error) {
	return i.BuildFn(cfg)
}

type mockDocker struct {
	inspect func(ctx context.Context, image string) (types.ImageInspect, []byte, error)
	build   func(ctx context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error)
}

func (m mockDocker) ImageInspectWithRaw(ctx context.Context, image string) (types.ImageInspect, []byte, error) {
	if m.inspect != nil {
		return m.inspect(ctx, image)
	}

	return types.ImageInspect{}, nil, nil
}

func (m mockDocker) ImageBuild(ctx context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
	if m.build != nil {
		return m.build(ctx, context, options)
	}

	_, _ = io.Copy(io.Discard, context)
	return types.ImageBuildResponse{
		Body:   io.NopCloser(strings.NewReader("")),
		OSType: "linux",
	}, nil
}

type notFoundErr struct {
}

func (n notFoundErr) Error() string {
	return "not found"
}

func (n notFoundErr) NotFound() bool {
	return true
}
