package function

import (
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/coreos/go-semver/semver"
	"gopkg.in/yaml.v2"
)

// Migrate applies any necessary migrations, returning a new migrated
// version of the Function.  It is the caller's responsibility to
// .Write() the Function to persist to disk.
func (f Function) Migrate() (migrated Function, err error) {
	// Return immediately if the Function indicates it has already been
	// migrated.
	if f.Migrated() {
		return f, nil
	}

	// If the version is empty, treat it as 0.0.0
	if f.Version == "" {
		f.Version = DefaultVersion
	}

	migrated = f // initially equivalent
	for _, m := range migrations {
		// Skip this migration if the current function's version is not less than
		// the migration's applicable version.
		if !semver.New(migrated.Version).LessThan(*semver.New(m.version)) {
			continue
		}
		// Apply this migration when the Function's version is less than that which
		// the migration will impart.
		migrated, err = m.migrate(migrated, m)
		if err != nil {
			return // fail fast on any migration errors
		}
	}
	return
}

// migration is a migration which should be applied to Functions whose version
// is below that indicated.
type migration struct {
	version string   // version before which this migration may be needed.
	migrate migrator // Migrator migrates.
}

// migrator is a function which returns a migrated copy of an inbound function.
type migrator func(Function, migration) (Function, error)

// Migrated returns whether or not the Function has been migrated to the highest
// level the currently executing system is aware of (or beyond).
// returns true.
func (f Function) Migrated() bool {
	// If the function has no Version, it is pre-migrations and is implicitly
	// not migrated.
	if f.Version == "" {
		return false
	}

	// lastMigration is the last registered migration.
	lastMigration := semver.New(migrations[len(migrations)-1].version)

	// Fail the migration test if the Function's version is less than
	// the latest available.
	return !semver.New(f.Version).LessThan(*lastMigration)
}

// Migrations registry
// -------------------

// migrations are all migrators in ascending order.
// No two migrations may have the exact version number (introduce a patch
// version for the migration if necessary)
var migrations = []migration{
	{"0.19.0", migrateToCreationStamp},
	{"0.23.0", migrateToBuilderImages},
	{"1.0.0", migrateTo100Structure},
	// New Migrations Here.
}

// Individual Migration implementations
// ------------------------------------

// migrateToCreationStamp
// The initial migration which brings a Function from
// some unknown point in the past to the point at which it is versioned,
// migrated and includes a creation timestamp.  Without this migration,
// instantiation of old functions will fail with a "Function at path X not
// initialized" in Func versions above v0.19.0
//
// This migration must be aware of the difference between a Function which
// was previously created (but with no create stamp), and a Function which
// exists only in memory and should legitimately fail the .Initialized() check.
// The only way to know is to check a side-effect of earlier versions:
// are the .Name and .Runtime fields populated.  This was the way the
// .Initialized check was implemented prior to versioning being introduced, so
// it is equivalent logically to use this here as well.

// In summary:  if the creation stamp is zero, but name and runtime fields are
// populated, then this is an old Function and should be migrated to having a
// create stamp.  Otherwise, this is an in-memory (new) Function that is
// currently in the process of being created and as such need not be mutated
// to consider this migration having been evaluated.
func migrateToCreationStamp(f Function, m migration) (Function, error) {
	// For functions with no creation timestamp, but appear to have been pre-
	// existing, populate their create stamp and version.
	// Yes, it's a little gnarly, but bootstrapping into the lovelieness of a
	// versioned/migrated system takes cleaning up the trash.
	if f.Created.IsZero() { // If there is no create stamp
		if f.Name != "" && f.Runtime != "" { // and it appears to be an old Function
			f.Created = time.Now() // Migrate it to having a timestamp.
		}
	}
	f.Version = m.version // Record this migration was evaluated.
	return f, nil
}

// migrateToBuilderImages
// Prior to this migration, 'builder' and 'builders' attributes of a Function
// were specific to buildpack builds.  In addition, the separation of the two
// fields was to facilitate the use of "named" inbuilt builders, which ended
// up not being necessary.  With the addition of the S2I builder implementation,
// it is necessary to differentiate builders for use when building via Pack vs
// builder for use when building with S2I.  Furthermore, now that the builder
// itself is a user-supplied parameter, the short-hand of calling builder images
// simply "builder" is not possible, since that term more correctly refers to
// the builder being used (S2I, pack, or some future implementation), while this
// field very specifically refers to the image the chosen builder should use
// (in leau of the inbuilt default).
//
// For an example of the situation:  the 'builder' member used to instruct the
// system to use that builder _image_ in all cases.  This of course restricts
// the system from being able to build with anything other than the builder
// implementation to which that builder image applies (pack or s2i).  Further,
// always including this value in the serialized func.yaml causes this value to
// be pegged/immutable (without manual intervention), which hampers our ability
// to change out the underlying builder image with future versions.
//
// The 'builder' and 'builders' members have therefore been removed in favor
// of 'builderImages', which is keyed by the short name of the builder
// implementation (currently 'pack' and 's2i').  Its existence is optional,
// with the default value being provided in the associated builder's impl.
// Should the value exist, this indicates the user has overridden the value,
// or is using a fully custom language pack.
//
// This migration allows pre-builder-image Functions to load despite their
// inclusion of the now removed 'builder' member.  If the user had provided
// a customized builder image, that value is preserved as the builder image
// for the 'pack' builder in the new version (s2i did not exist prior).
// See associated unit tests.
func migrateToBuilderImages(f1 Function, m migration) (Function, error) {
	// Load the Function using pertinent parts of the previous version's schema:
	f0Filename := filepath.Join(f1.Root, FunctionFile)
	bb, err := ioutil.ReadFile(f0Filename)
	if err != nil {
		return f1, err
	}
	f0 := migrateToBuilderImages_previousFunction{}
	if err = yaml.Unmarshal(bb, &f0); err != nil {
		return f1, err
	}

	// At time of this migration, the default pack builder image for all language
	// runtimes is:
	defaultPackBuilderImage := "gcr.io/paketo-buildpacks/builder:base"

	// If the old Function had defined something custom
	if f0.Builder != "" && f0.Builder != defaultPackBuilderImage {
		// carry it forward as the new pack builder image
		if f1.Build.BuilderImages == nil {
			f1.Build.BuilderImages = make(map[string]string)
		}
		f1.Build.BuilderImages["pack"] = f0.Builder
	}

	// Flag f1 as having had the migration applied
	f1.Version = m.version
	return f1, nil

}

func migrateTo100Structure(f1 Function, m migration) (Function, error) {
	// Load the Function using pertinent parts of the previous version's schema:
	f0Filename := filepath.Join(f1.Root, FunctionFile)
	bb, err := ioutil.ReadFile(f0Filename)
	if err != nil {
		return f1, err
	}
	f0 := migrateTo100_previousFunction{}
	if err = yaml.Unmarshal(bb, &f0); err != nil {
		return f1, err
	}

	f1.Build.Git = f0.Git
	f1.Build.BuildType = f0.BuildType
	f1.Build.BuilderImages = f0.BuilderImages
	f1.Build.Buildpacks = f0.Buildpacks
	f1.Build.BuildEnvs = f0.BuildEnvs
	f1.Run.Namespace = f0.Namespace
	f1.Run.Volumes = f0.Volumes
	f1.Run.Envs = f0.Envs
	f1.Run.Annotations = f0.Annotations
	f1.Run.Options = f0.Options
	f1.Run.Labels = f0.Labels
	f1.Run.HealthEndpoints = f0.HealthEndpoints

	f1.Version = m.version
	return f1, nil
}

// The pertinent aspects of the Function schema prior to the builder images
// migration.
type migrateToBuilderImages_previousFunction struct {
	Builder string `yaml:"builder"`
}

// The pertinent aspects of the Functions schema prior the 1.0.0 version migrations
type migrateTo100_previousFunction struct {
	// New Build Section

	// BuildType represents the specified way of building the fuction
	// ie. "local" or "git"
	BuildType string `yaml:"build" jsonschema:"enum=local,enum=git"`

	// Git stores information about remote git repository,
	// in case build type "git" is being used
	Git Git `yaml:"git"`

	// BuilderImages define optional explicit builder images to use by
	// builder implementations in leau of the in-code defaults.  They key
	// is the builder's short name.  For example:
	// builderImages:
	//   pack: example.com/user/my-pack-node-builder
	//   s2i: example.com/user/my-s2i-node-builder
	BuilderImages map[string]string `yaml:"builderImages,omitempty"`

	// Optional list of buildpacks to use when building the function
	Buildpacks []string `yaml:"buildpacks"`

	// Builder is the name of the subsystem that will complete the underlying
	// build (pack, s2i, etc)
	Builder string `yaml:"builder" jsonschema:"enum=pack,enum=s2i"`

	// New Run Section

	//Namespace into which the function is deployed on supported platforms.
	Namespace string `yaml:"namespace"`

	// List of volumes to be mounted to the function
	Volumes []Volume `yaml:"volumes"`

	// Build Env variables to be set
	BuildEnvs []Env `yaml:"buildEnvs"`

	// Env variables to be set
	Envs []Env `yaml:"envs"`

	// Map containing user-supplied annotations
	// Example: { "division": "finance" }
	Annotations map[string]string `yaml:"annotations"`

	// Options to be set on deployed function (scaling, etc.)
	Options Options `yaml:"options"`

	// Map of user-supplied labels
	Labels []Label `yaml:"labels"`

	// Health endpoints specified by the language pack
	HealthEndpoints HealthEndpoints `yaml:"healthEndpoints"`
}
