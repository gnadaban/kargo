package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum={Lexical,NewestFromBranch,NewestTag,SemVer}
type CommitSelectionStrategy string

const (
	CommitSelectionStrategyLexical          CommitSelectionStrategy = "Lexical"
	CommitSelectionStrategyNewestFromBranch CommitSelectionStrategy = "NewestFromBranch"
	CommitSelectionStrategyNewestTag        CommitSelectionStrategy = "NewestTag"
	CommitSelectionStrategySemVer           CommitSelectionStrategy = "SemVer"
)

// +kubebuilder:validation:Enum={Digest,Lexical,NewestBuild,SemVer}
type ImageSelectionStrategy string

const (
	ImageSelectionStrategyDigest      ImageSelectionStrategy = "Digest"
	ImageSelectionStrategyLexical     ImageSelectionStrategy = "Lexical"
	ImageSelectionStrategyNewestBuild ImageSelectionStrategy = "NewestBuild"
	ImageSelectionStrategySemVer      ImageSelectionStrategy = "SemVer"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name=Shard,type=string,JSONPath=`.spec.shard`
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=`.metadata.creationTimestamp`

// Warehouse is a source of Freight.
type Warehouse struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// Spec describes sources of artifacts.
	//
	// +kubebuilder:validation:Required
	Spec WarehouseSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	// Status describes the Warehouse's most recently observed state.
	Status WarehouseStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// GetInterval calculates and returns interval time remaining until the next
// requeue should occur. If the interval has passed, it returns a short duration
// to ensure the Warehouse is requeued promptly.
func (w *Warehouse) GetInterval(minInterval time.Duration) time.Duration {
	effectiveInterval := w.Spec.Interval.Duration
	if effectiveInterval < minInterval {
		effectiveInterval = minInterval
	}

	if w.Status.DiscoveredArtifacts == nil || w.Status.DiscoveredArtifacts.DiscoveredAt.IsZero() {
		return effectiveInterval
	}

	if interval := w.Status.DiscoveredArtifacts.DiscoveredAt.
		Add(effectiveInterval).
		Sub(metav1.Now().Time); interval > 0 {
		return interval
	}
	return 100 * time.Millisecond
}

func (w *Warehouse) GetStatus() *WarehouseStatus {
	return &w.Status
}

// WarehouseSpec describes sources of versioned artifacts to be included in
// Freight produced by this Warehouse.
type WarehouseSpec struct {
	// Shard is the name of the shard that this Warehouse belongs to. This is an
	// optional field. If not specified, the Warehouse will belong to the default
	// shard. A defaulting webhook will sync this field with the value of the
	// kargo.akuity.io/shard label. When the shard label is not present or differs
	// from the value of this field, the defaulting webhook will set the label to
	// the value of this field. If the shard label is present and this field is
	// empty, the defaulting webhook will set the value of this field to the value
	// of the shard label.
	Shard string `json:"shard,omitempty" protobuf:"bytes,2,opt,name=shard"`
	// Interval is the reconciliation interval for this Warehouse. On each
	// reconciliation, the Warehouse will discover new artifacts and optionally
	// produce new Freight. This field is optional. When left unspecified, the
	// field is implicitly treated as if its value were "5m0s".
	//
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:default="5m0s"
	// +akuity:test-kubebuilder-pattern=Duration
	Interval metav1.Duration `json:"interval" protobuf:"bytes,4,opt,name=interval"`
	// FreightCreationPolicy describes how Freight is created by this Warehouse.
	// This field is optional. When left unspecified, the field is implicitly
	// treated as if its value were "Automatic".
	//
	// Accepted values:
	//
	// - "Automatic": New Freight is created automatically when any new artifact
	//   is discovered.
	// - "Manual": New Freight is never created automatically.
	//
	// +kubebuilder:default=Automatic
	// +kubebuilder:validation:Optional
	FreightCreationPolicy FreightCreationPolicy `json:"freightCreationPolicy" protobuf:"bytes,3,opt,name=freightCreationPolicy"`
	// Subscriptions describes sources of artifacts to be included in Freight
	// produced by this Warehouse.
	//
	// +kubebuilder:validation:MinItems=1
	Subscriptions []RepoSubscription `json:"subscriptions" protobuf:"bytes,1,rep,name=subscriptions"`
}

// FreightCreationPolicy defines how Freight is created by a Warehouse.
// +kubebuilder:validation:Enum={Automatic,Manual}
type FreightCreationPolicy string

const (
	// FreightCreationPolicyAutomatic indicates that Freight is created automatically.
	FreightCreationPolicyAutomatic FreightCreationPolicy = "Automatic"
	// FreightCreationPolicyManual indicates that Freight is created manually.
	FreightCreationPolicyManual FreightCreationPolicy = "Manual"
)

// RepoSubscription describes a subscription to ONE OF a Git repository, a
// container image repository, or a Helm chart repository.
type RepoSubscription struct {
	// Git describes a subscriptions to a Git repository.
	Git *GitSubscription `json:"git,omitempty" protobuf:"bytes,1,opt,name=git"`
	// Image describes a subscription to container image repository.
	Image *ImageSubscription `json:"image,omitempty" protobuf:"bytes,2,opt,name=image"`
	// Chart describes a subscription to a Helm chart repository.
	Chart *ChartSubscription `json:"chart,omitempty" protobuf:"bytes,3,opt,name=chart"`
}

// GitSubscription defines a subscription to a Git repository.
type GitSubscription struct {
	// URL is the repository's URL. This is a required field.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`(?:^(ssh|https?)://(?:([\w-]+)(:(.+))?@)?([\w-]+(?:\.[\w-]+)*)(?::(\d{1,5}))?(/.*)$)|(?:^([\w-]+)@([\w+]+(?:\.[\w-]+)*):(/?.*))`
	// +akuity:test-kubebuilder-pattern=GitRepoURLPattern
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// CommitSelectionStrategy specifies the rules for how to identify the newest
	// commit of interest in the repository specified by the RepoURL field. This
	// field is optional. When left unspecified, the field is implicitly treated
	// as if its value were "NewestFromBranch".
	//
	// Accepted values:
	//
	// - "NewestFromBranch": Selects the latest commit on the branch specified
	//   by the Branch field or the default branch if none is specified. This is
	//   the default strategy.
	//
	// - "SemVer": Selects the commit referenced by the semantically greatest
	//   tag. The SemverConstraint field can optionally be used to narrow the set
	//   of tags eligible for selection.
	//
	// - "Lexical": Selects the commit referenced by the lexicographically
	//   greatest tag. Useful when tags embed a _leading_ date or timestamp. The
	//   AllowTags and IgnoreTags fields can optionally be used to narrow the set
	//   of tags eligible for selection.
	//
	// - "NewestTag": Selects the commit referenced by the most recently created
	//   tag. The AllowTags and IgnoreTags fields can optionally be used to
	//   narrow the set of tags eligible for selection.
	//
	// +kubebuilder:default=NewestFromBranch
	CommitSelectionStrategy CommitSelectionStrategy `json:"commitSelectionStrategy,omitempty" protobuf:"bytes,2,opt,name=commitSelectionStrategy"`
	// Branch references a particular branch of the repository. The value in this
	// field only has any effect when the CommitSelectionStrategy is
	// NewestFromBranch or left unspecified (which is implicitly the same as
	// NewestFromBranch). This field is optional. When left unspecified, (and the
	// CommitSelectionStrategy is NewestFromBranch or unspecified), the
	// subscription is implicitly to the repository's default branch.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9._\/-]*[a-zA-Z0-9_-])?$`
	// +akuity:test-kubebuilder-pattern=Branch
	Branch string `json:"branch,omitempty" protobuf:"bytes,3,opt,name=branch"`
	// StrictSemvers specifies whether only "strict" semver tags should be
	// considered. A "strict" semver tag is one containing ALL of major, minor,
	// and patch version components. This is enabled by default, but only has any
	// effect when the CommitSelectionStrategy is SemVer. This should be disabled
	// cautiously, as it creates the potential for any tag containing numeric
	// characters only to be mistaken for a semver string containing the major
	// version number only.
	//
	// +kubebuilder:default=true
	StrictSemvers bool `json:"strictSemvers" protobuf:"varint,11,opt,name=strictSemvers"`
	// SemverConstraint specifies constraints on what new tagged commits are
	// considered in determining the newest commit of interest. The value in this
	// field only has any effect when the CommitSelectionStrategy is SemVer. This
	// field is optional. When left unspecified, there will be no constraints,
	// which means the latest semantically tagged commit will always be used. Care
	// should be taken with leaving this field unspecified, as it can lead to the
	// unanticipated rollout of breaking changes.
	//
	// +kubebuilder:validation:Optional
	SemverConstraint string `json:"semverConstraint,omitempty" protobuf:"bytes,4,opt,name=semverConstraint"`
	// AllowTags is a regular expression that can optionally be used to limit the
	// tags that are considered in determining the newest commit of interest. The
	// value in this field only has any effect when the CommitSelectionStrategy is
	// Lexical, NewestTag, or SemVer. This field is optional.
	//
	// +kubebuilder:validation:Optional
	AllowTags string `json:"allowTags,omitempty" protobuf:"bytes,5,opt,name=allowTags"`
	// IgnoreTags is a list of tags that must be ignored when determining the
	// newest commit of interest. No regular expressions or glob patterns are
	// supported yet. The value in this field only has any effect when the
	// CommitSelectionStrategy is Lexical, NewestTag, or SemVer. This field is
	// optional.
	//
	// +kubebuilder:validation:Optional
	IgnoreTags []string `json:"ignoreTags,omitempty" protobuf:"bytes,6,rep,name=ignoreTags"`
	// ExpressionFilter is an expression that can optionally be used to limit
	// the commits or tags that are considered in determining the newest commit
	// of interest based on their metadata.
	//
	// For commit-based strategies (NewestFromBranch), the filter applies to
	// commits and has access to commit metadata variables.
	// For tag-based strategies (Lexical, NewestTag, SemVer), the filter applies
	// to tags and has access to tag metadata variables. The filter is applied
	// after AllowTags, IgnoreTags, and SemverConstraint fields.
	//
	// The expression should be a valid expr-lang expression that evaluates to
	// true or false. When the expression evaluates to true, the commit/tag is
	// included in the set that is considered. When the expression evaluates to
	// false, the commit/tag is excluded.
	//
	// Available variables depend on the CommitSelectionStrategy:
	//
	// For NewestFromBranch (commit filtering):
	//   - `id`: The ID (sha) of the commit.
	//   - `commitDate`: The commit date of the commit.
	//   - `author`: The author of the commit message, in the format "Name <email>".
	//   - `committer`: The person who committed the commit, in the format
	//	   "Name <email>".
	//   - `subject`: The subject (first line) of the commit message.
	//
	// For Lexical, NewestTag, SemVer (tag filtering):
	//   - `tag`: The name of the tag.
	//   - `id`: The ID (sha) of the commit associated with the tag.
	//   - `creatorDate`: The creation date of an annotated tag, or the commit
	//		date of a lightweight tag.
	//   - `author`: The author of the commit message associated with the tag,
	//	   in the format "Name <email>".
	//   - `committer`: The person who committed the commit associated with the
	//	   tag, in the format "Name <email>".
	//   - `subject`: The subject (first line) of the commit message associated
	//	   with the tag.
	//	 - `tagger`: The person who created the tag, in the format "Name <email>".
	//	   Only available for annotated tags.
	//	 - `annotation`: The subject (first line) of the tag annotation. Only
	//	   available for annotated tags.
	//
	// Refer to the expr-lang documentation for more details on syntax and
	// capabilities of the expression language: https://expr-lang.org.
	//
	// +kubebuilder:validation:Optional
	ExpressionFilter string `json:"expressionFilter,omitempty" protobuf:"bytes,12,opt,name=expressionFilter"`
	// InsecureSkipTLSVerify specifies whether certificate verification errors
	// should be ignored when connecting to the repository. This should be enabled
	// only with great caution.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty" protobuf:"varint,7,opt,name=insecureSkipTLSVerify"`
	// IncludePaths is a list of selectors that designate paths in the repository
	// that should trigger the production of new Freight when changes are detected
	// therein. When specified, only changes in the identified paths will trigger
	// Freight production. When not specified, changes in any path will trigger
	// Freight production. Selectors may be defined using:
	//   1. Exact paths to files or directories (ex. "charts/foo")
	//   2. Glob patterns (prefix the pattern with "glob:"; ex. "glob:*.yaml")
	//   3. Regular expressions (prefix the pattern with "regex:" or "regexp:";
	//      ex. "regexp:^.*\.yaml$")
	//
	// Paths selected by IncludePaths may be unselected by ExcludePaths. This
	// is a useful method for including a broad set of paths and then excluding a
	// subset of them.
	// +kubebuilder:validation:Optional
	IncludePaths []string `json:"includePaths,omitempty" protobuf:"bytes,8,rep,name=includePaths"`
	// ExcludePaths is a list of selectors that designate paths in the repository
	// that should NOT trigger the production of new Freight when changes are
	// detected therein. When specified, changes in the identified paths will not
	// trigger Freight production. When not specified, paths that should trigger
	// Freight production will be defined solely by IncludePaths. Selectors may be
	// defined using:
	//   1. Exact paths to files or directories (ex. "charts/foo")
	//   2. Glob patterns (prefix the pattern with "glob:"; ex. "glob:*.yaml")
	//   3. Regular expressions (prefix the pattern with "regex:" or "regexp:";
	//      ex. "regexp:^.*\.yaml$")
	// Paths selected by IncludePaths may be unselected by ExcludePaths. This
	// is a useful method for including a broad set of paths and then excluding a
	// subset of them.
	// +kubebuilder:validation:Optional
	ExcludePaths []string `json:"excludePaths,omitempty" protobuf:"bytes,9,rep,name=excludePaths"`
	// DiscoveryLimit is an optional limit on the number of commits that can be
	// discovered for this subscription. The limit is applied after filtering
	// commits based on the AllowTags and IgnoreTags fields.
	// When left unspecified, the field is implicitly treated as if its value
	// were "20". The upper limit for this field is 100.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	DiscoveryLimit int32 `json:"discoveryLimit,omitempty" protobuf:"varint,10,opt,name=discoveryLimit"`
}

// ImageSubscription defines a subscription to an image repository.
//
// +kubebuilder:validation:XValidation:message="semverConstraint and constraint fields are mutually exclusive",rule="!(has(self.semverConstraint) && has(self.constraint))"
// +kubebuilder:validation:XValidation:message="If imageSelectionStrategy is Digest, either constraint or semverConstraint must be set",rule="!(self.imageSelectionStrategy == 'Digest') || has(self.constraint) || has(self.semverConstraint)"
type ImageSubscription struct {
	// RepoURL specifies the URL of the image repository to subscribe to. The
	// value in this field MUST NOT include an image tag. This field is required.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(\w+([\.-]\w+)*(:[\d]+)?/)?(\w+([\.-]\w+)*)(/\w+([\.-]\w+)*)*$`
	// +akuity:test-kubebuilder-pattern=ImageRepoURL
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// ImageSelectionStrategy specifies the rules for how to identify the newest version
	// of the image specified by the RepoURL field. This field is optional. When
	// left unspecified, the field is implicitly treated as if its value were
	// "SemVer".
	//
	// Accepted values:
	//
	// - "Digest": Selects the image currently referenced by the tag specified
	//   (unintuitively) by the SemverConstraint field.
	//
	// - "Lexical": Selects the image referenced by the lexicographically greatest
	//   tag. Useful when tags embed a leading date or timestamp. The AllowTags
	//   and IgnoreTags fields can optionally be used to narrow the set of tags
	//   eligible for selection.
	//
	// - "NewestBuild": Selects the image that was most recently pushed to the
	//   repository. The AllowTags and IgnoreTags fields can optionally be used
	//   to narrow the set of tags eligible for selection. This is the least
	//   efficient and is likely to cause rate limiting affecting this Warehouse
	//   and possibly others. This strategy should be avoided.
	//
	// - "SemVer": Selects the image with the semantically greatest tag. The
	//   AllowTags and IgnoreTags fields can optionally be used to narrow the set
	//   of tags eligible for selection.
	//
	// +kubebuilder:default=SemVer
	ImageSelectionStrategy ImageSelectionStrategy `json:"imageSelectionStrategy,omitempty" protobuf:"bytes,3,opt,name=imageSelectionStrategy"`
	// StrictSemvers specifies whether only "strict" semver tags should be
	// considered. A "strict" semver tag is one containing ALL of major, minor,
	// and patch version components. This is enabled by default, but only has any
	// effect when the ImageSelectionStrategy is SemVer. This should be disabled
	// cautiously, as it is not uncommon to tag container images with short Git
	// commit hashes, which have the potential to contain numeric characters only
	// and could be mistaken for a semver string containing the major version
	// number only.
	//
	// +kubebuilder:default=true
	StrictSemvers bool `json:"strictSemvers" protobuf:"varint,10,opt,name=strictSemvers"`
	// SemverConstraint specifies constraints on what new image versions are
	// permissible. The value in this field only has any effect when the
	// ImageSelectionStrategy is SemVer or left unspecified (which is implicitly
	// the same as SemVer). This field is also optional. When left unspecified,
	// (and the ImageSelectionStrategy is SemVer or unspecified), there will be no
	// constraints, which means the latest semantically tagged version of an image
	// will always be used. Care should be taken with leaving this field
	// unspecified, as it can lead to the unanticipated rollout of breaking
	// changes.
	// More info: https://github.com/masterminds/semver#checking-version-constraints
	//
	// Deprecated: Use Constraint instead. This field will be removed in v1.9.0
	//
	// +kubebuilder:validation:Optional
	SemverConstraint string `json:"semverConstraint,omitempty" protobuf:"bytes,4,opt,name=semverConstraint"`
	// Constraint specifies constraints on what new image versions are
	// permissible. Acceptable values for this field vary contextually by
	// ImageSelectionStrategy. The field is optional and is ignored by some
	// strategies. When non-empty, the value in this field takes precedence over
	// the value of the deprecated SemverConstraint field.
	//
	// +kubebuilder:validation:Optional
	Constraint string `json:"constraint,omitempty" protobuf:"bytes,11,opt,name=constraint"`
	// AllowTags is a regular expression that can optionally be used to limit the
	// image tags that are considered in determining the newest version of an
	// image. This field is optional.
	//
	// +kubebuilder:validation:Optional
	AllowTags string `json:"allowTags,omitempty" protobuf:"bytes,5,opt,name=allowTags"`
	// IgnoreTags is a list of tags that must be ignored when determining the
	// newest version of an image. No regular expressions or glob patterns are
	// supported yet. This field is optional.
	//
	// +kubebuilder:validation:Optional
	IgnoreTags []string `json:"ignoreTags,omitempty" protobuf:"bytes,6,rep,name=ignoreTags"`
	// Platform is a string of the form <os>/<arch> that limits the tags that can
	// be considered when searching for new versions of an image. This field is
	// optional. When left unspecified, it is implicitly equivalent to the
	// OS/architecture of the Kargo controller. Care should be taken to set this
	// value correctly in cases where the image referenced by this
	// ImageRepositorySubscription will run on a Kubernetes node with a different
	// OS/architecture than the Kargo controller. At present this is uncommon, but
	// not unheard of.
	//
	// +kubebuilder:validation:Optional
	Platform string `json:"platform,omitempty" protobuf:"bytes,7,opt,name=platform"`
	// InsecureSkipTLSVerify specifies whether certificate verification errors
	// should be ignored when connecting to the repository. This should be enabled
	// only with great caution.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty" protobuf:"varint,8,opt,name=insecureSkipTLSVerify"`
	// DiscoveryLimit is an optional limit on the number of image references
	// that can be discovered for this subscription. The limit is applied after
	// filtering images based on the AllowTags and IgnoreTags fields.
	// When left unspecified, the field is implicitly treated as if its value
	// were "20". The upper limit for this field is 100.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	DiscoveryLimit int32 `json:"discoveryLimit,omitempty" protobuf:"varint,9,opt,name=discoveryLimit"`
}

// ChartSubscription defines a subscription to a Helm chart repository.
type ChartSubscription struct {
	// RepoURL specifies the URL of a Helm chart repository. It may be a classic
	// chart repository (using HTTP/S) OR a repository within an OCI registry.
	// Classic chart repositories can contain differently named charts. When this
	// field points to such a repository, the Name field MUST also be used
	// to specify the name of the desired chart within that repository. In the
	// case of a repository within an OCI registry, the URL implicitly points to
	// a specific chart and the Name field MUST NOT be used. The RepoURL field is
	// required.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(((https?)|(oci))://)([\w\d\.\-]+)(:[\d]+)?(/.*)*$`
	// +akuity:test-kubebuilder-pattern=HelmRepoURL
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// Name specifies the name of a Helm chart to subscribe to within a classic
	// chart repository specified by the RepoURL field. This field is required
	// when the RepoURL field points to a classic chart repository and MUST
	// otherwise be empty.
	Name string `json:"name,omitempty" protobuf:"bytes,2,opt,name=name"`
	// SemverConstraint specifies constraints on what new chart versions are
	// permissible. This field is optional. When left unspecified, there will be
	// no constraints, which means the latest version of the chart will always be
	// used. Care should be taken with leaving this field unspecified, as it can
	// lead to the unanticipated rollout of breaking changes.
	// More info: https://github.com/masterminds/semver#checking-version-constraints
	//
	// +kubebuilder:validation:Optional
	SemverConstraint string `json:"semverConstraint,omitempty" protobuf:"bytes,3,opt,name=semverConstraint"`
	// DiscoveryLimit is an optional limit on the number of chart versions that
	// can be discovered for this subscription. The limit is applied after
	// filtering charts based on the SemverConstraint field.
	// When left unspecified, the field is implicitly treated as if its value
	// were "20". The upper limit for this field is 100.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	DiscoveryLimit int32 `json:"discoveryLimit,omitempty" protobuf:"varint,4,opt,name=discoveryLimit"`
}

// WarehouseStatus describes a Warehouse's most recently observed state.
type WarehouseStatus struct {
	// Conditions contains the last observations of the Warehouse's current
	// state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchMergeKey:"type" patchStrategy:"merge" protobuf:"bytes,9,rep,name=conditions"`
	// LastHandledRefresh holds the value of the most recent AnnotationKeyRefresh
	// annotation that was handled by the controller. This field can be used to
	// determine whether the request to refresh the resource has been handled.
	// +optional
	LastHandledRefresh string `json:"lastHandledRefresh,omitempty" protobuf:"bytes,6,opt,name=lastHandledRefresh"`
	// ObservedGeneration represents the .metadata.generation that this Warehouse
	// was reconciled against.
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,4,opt,name=observedGeneration"`
	// LastFreightID is a reference to the system-assigned identifier (name) of
	// the most recent Freight produced by the Warehouse.
	LastFreightID string `json:"lastFreightID,omitempty" protobuf:"bytes,8,opt,name=lastFreightID"`
	// DiscoveredArtifacts holds the artifacts discovered by the Warehouse.
	DiscoveredArtifacts *DiscoveredArtifacts `json:"discoveredArtifacts,omitempty" protobuf:"bytes,7,opt,name=discoveredArtifacts"`
}

// GetConditions implements the conditions.Getter interface.
func (w *WarehouseStatus) GetConditions() []metav1.Condition {
	return w.Conditions
}

// SetConditions implements the conditions.Setter interface.
func (w *WarehouseStatus) SetConditions(conditions []metav1.Condition) {
	w.Conditions = conditions
}

// DiscoveredArtifacts holds the artifacts discovered by the Warehouse for its
// subscriptions.
type DiscoveredArtifacts struct {
	// DiscoveredAt is the time at which the Warehouse discovered the artifacts.
	//
	// +optional
	DiscoveredAt metav1.Time `json:"discoveredAt" protobuf:"bytes,4,opt,name=discoveredAt"`
	// Git holds the commits discovered by the Warehouse for the Git
	// subscriptions.
	//
	// +optional
	Git []GitDiscoveryResult `json:"git,omitempty" protobuf:"bytes,1,rep,name=git"`
	// Images holds the image references discovered by the Warehouse for the
	// image subscriptions.
	//
	// +optional
	Images []ImageDiscoveryResult `json:"images,omitempty" protobuf:"bytes,2,rep,name=images"`
	// Charts holds the charts discovered by the Warehouse for the chart
	// subscriptions.
	//
	// +optional
	Charts []ChartDiscoveryResult `json:"charts,omitempty" protobuf:"bytes,3,rep,name=charts"`
}

// GitDiscoveryResult represents the result of a Git discovery operation for a
// GitSubscription.
type GitDiscoveryResult struct {
	// RepoURL is the repository URL of the GitSubscription.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`(?:^(ssh|https?)://(?:([\w-]+)(:(.+))?@)?([\w-]+(?:\.[\w-]+)*)(?::(\d{1,5}))?(/.*)$)|(?:^([\w-]+)@([\w+]+(?:\.[\w-]+)*):(/?.*))`
	// +akuity:test-kubebuilder-pattern=GitRepoURLPattern
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// Commits is a list of commits discovered by the Warehouse for the
	// GitSubscription. An empty list indicates that the discovery operation was
	// successful, but no commits matching the GitSubscription criteria were found.
	//
	// +optional
	Commits []DiscoveredCommit `json:"commits" protobuf:"bytes,2,rep,name=commits"`
}

// DiscoveredCommit represents a commit discovered by a Warehouse for a
// GitSubscription.
type DiscoveredCommit struct {
	// ID is the identifier of the commit. This typically is a SHA-1 hash.
	//
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id,omitempty" protobuf:"bytes,1,opt,name=id"`
	// Branch is the branch in which the commit was found. This field is
	// optional, and populated based on the CommitSelectionStrategy of the
	// GitSubscription.
	Branch string `json:"branch,omitempty" protobuf:"bytes,2,opt,name=branch"`
	// Tag is the tag that resolved to this commit. This field is optional, and
	// populated based on the CommitSelectionStrategy of the GitSubscription.
	Tag string `json:"tag,omitempty" protobuf:"bytes,3,opt,name=tag"`
	// Subject is the subject of the commit (i.e. the first line of the commit
	// message).
	Subject string `json:"subject,omitempty" protobuf:"bytes,4,opt,name=subject"`
	// Author is the author of the commit.
	Author string `json:"author,omitempty" protobuf:"bytes,5,opt,name=author"`
	// Committer is the person who committed the commit.
	Committer string `json:"committer,omitempty" protobuf:"bytes,6,opt,name=committer"`
	// CreatorDate is the commit creation date as specified by the commit, or
	// the tagger date if the commit belongs to an annotated tag.
	CreatorDate *metav1.Time `json:"creatorDate,omitempty" protobuf:"bytes,7,opt,name=creatorDate"`
}

// ImageDiscoveryResult represents the result of an image discovery operation
// for an ImageSubscription.
type ImageDiscoveryResult struct {
	// RepoURL is the repository URL of the image, as specified in the
	// ImageSubscription.
	//
	// +kubebuilder:validation:MinLength=1
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// Platform is the target platform constraint of the ImageSubscription
	// for which references were discovered. This field is optional, and
	// only populated if the ImageSubscription specifies a Platform.
	Platform string `json:"platform,omitempty" protobuf:"bytes,2,opt,name=platform"`
	// References is a list of image references discovered by the Warehouse for
	// the ImageSubscription. An empty list indicates that the discovery
	// operation was successful, but no images matching the ImageSubscription
	// criteria were found.
	//
	// +optional
	References []DiscoveredImageReference `json:"references" protobuf:"bytes,3,rep,name=references"`
}

// DiscoveredImageReference represents an image reference discovered by a
// Warehouse for an ImageSubscription.
type DiscoveredImageReference struct {
	// Tag is the tag of the image.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[\w.\-\_]+$`
	// +akuity:test-kubebuilder-pattern=Tag
	Tag string `json:"tag" protobuf:"bytes,1,opt,name=tag"`
	// Digest is the digest of the image.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+:[a-f0-9]+$`
	// +akuity:test-kubebuilder-pattern=Digest
	Digest string `json:"digest" protobuf:"bytes,2,opt,name=digest"`
	// Annotations is a map of key-value pairs that provide additional
	// information about the image.
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,5,rep,name=annotations" protobuf_key:"bytes,1,opt,name=key" protobuf_val:"bytes,2,opt,name=value"`
	// CreatedAt is the time the image was created. This field is optional, and
	// not populated for every ImageSelectionStrategy.
	CreatedAt *metav1.Time `json:"createdAt,omitempty" protobuf:"bytes,4,opt,name=createdAt"`
}

// ChartDiscoveryResult represents the result of a chart discovery operation for
// a ChartSubscription.
type ChartDiscoveryResult struct {
	// RepoURL is the repository URL of the Helm chart, as specified in the
	// ChartSubscription.
	//
	// +kubebuilder:validation:MinLength=1
	RepoURL string `json:"repoURL" protobuf:"bytes,1,opt,name=repoURL"`
	// Name is the name of the Helm chart, as specified in the ChartSubscription.
	Name string `json:"name,omitempty" protobuf:"bytes,2,opt,name=name"`
	// SemverConstraint is the constraint for which versions were discovered.
	// This field is optional, and only populated if the ChartSubscription
	// specifies a SemverConstraint.
	SemverConstraint string `json:"semverConstraint,omitempty" protobuf:"bytes,3,opt,name=semverConstraint"`
	// Versions is a list of versions discovered by the Warehouse for the
	// ChartSubscription. An empty list indicates that the discovery operation was
	// successful, but no versions matching the ChartSubscription criteria were
	// found.
	//
	// +optional
	Versions []string `json:"versions" protobuf:"bytes,4,rep,name=versions"`
}

// +kubebuilder:object:root=true

// WarehouseList is a list of Warehouse resources.
type WarehouseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items           []Warehouse `json:"items" protobuf:"bytes,2,rep,name=items"`
}
