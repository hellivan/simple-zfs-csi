package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsPoolScrubPhase is the coarse state of one scrub run.
type ZfsPoolScrubPhase string

const (
	// ScrubPhasePending means the operator has not created the Job yet.
	ScrubPhasePending ZfsPoolScrubPhase = "Pending"
	// ScrubPhaseRunning means the scrub Job exists and has not finished.
	ScrubPhaseRunning ZfsPoolScrubPhase = "Running"
	// ScrubPhaseSucceeded means the scrub completed cleanly.
	ScrubPhaseSucceeded ZfsPoolScrubPhase = "Succeeded"
	// ScrubPhaseFailed means the scrub could not be run or did not complete
	// cleanly. It is terminal: a failed scrub is never retried (ADR-0044).
	ScrubPhaseFailed ZfsPoolScrubPhase = "Failed"
)

// ZfsPoolScrubJobSpec requests one scrub of the pool identified by PoolGUID.
// The CR must live in the operator's namespace. See design-decisions ADR-0044.
type ZfsPoolScrubJobSpec struct {
	// PoolGUID is the ZFS pool GUID (the ZfsPool metadata.name without the
	// "zpool-" prefix).
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Timeout is passed to the scrub binary as its wait limit. When unset, the
	// chart's maintenance.scrub.defaults.timeout applies. Keep it below
	// ActiveDeadlineSeconds.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// ActiveDeadlineSeconds is the Kubernetes Job's wall-clock limit. When unset,
	// the chart's maintenance.scrub.defaults.activeDeadlineSeconds applies.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// TTLSecondsAfterFinished deletes the Job (not this CR) that long after it
	// finishes. The CR keeps the result in its status.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// ZfsPoolScrubJobStatus reports the observed state of the scrub run.
type ZfsPoolScrubJobStatus struct {
	// Phase is the coarse state of the run. Succeeded and Failed are terminal.
	// +optional
	Phase ZfsPoolScrubPhase `json:"phase,omitempty"`

	// JobName is the name of the Kubernetes Job running the scrub.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// StartTime is when the Job started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message is human-readable detail. For a failed run it carries the tail of
	// the scrub container's output (for example "currently scrubbing" when ZFS
	// refused because a scrub was already running).
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=zpscrubjob
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsPoolScrubJob is one scrub run of a ZFS pool. The operator creates a
// Kubernetes Job from it, pinned to the node currently hosting the pool, and
// mirrors the outcome into status. ZFS itself refuses a scrub while another
// scrub or resilver is running, so an overlapping run simply fails.
type ZfsPoolScrubJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsPoolScrubJobSpec   `json:"spec,omitempty"`
	Status ZfsPoolScrubJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsPoolScrubJobList contains a list of ZfsPoolScrubJob.
type ZfsPoolScrubJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsPoolScrubJob `json:"items"`
}

// ZfsPoolScrubCronJobSpec schedules recurring scrubs of one pool. The CR must
// live in the operator's namespace. See design-decisions ADR-0044.
type ZfsPoolScrubCronJobSpec struct {
	// PoolGUID is the ZFS pool GUID (the ZfsPool metadata.name without the
	// "zpool-" prefix).
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Schedule is a cron expression, as for a Kubernetes CronJob.
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// Suspend stops new scrubs from being scheduled. The operator also suspends
	// the CronJob by itself while the pool has no reachable node.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// StartingDeadlineSeconds is how late a missed start may still run. When
	// unset, the chart's maintenance.scrub.defaults.startingDeadlineSeconds applies.
	// +optional
	// +kubebuilder:validation:Minimum=0
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// Timeout is passed to the scrub binary as its wait limit. When unset, the
	// chart's maintenance.scrub.defaults.timeout applies.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// ActiveDeadlineSeconds is each Job's wall-clock limit. When unset, the
	// chart's maintenance.scrub.defaults.activeDeadlineSeconds applies.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// SuccessfulJobsHistoryLimit overrides the chart default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// FailedJobsHistoryLimit overrides the chart default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// ZfsPoolScrubCronJobStatus reports the observed state of the scheduled scrub.
type ZfsPoolScrubCronJobStatus struct {
	// CronJobName is the name of the Kubernetes CronJob the operator maintains.
	// +optional
	CronJobName string `json:"cronJobName,omitempty"`

	// Suspended is whether the CronJob is currently suspended, either by
	// spec.suspend or because the pool has no reachable node.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// CurrentNode is the node the CronJob is currently pinned to.
	// +optional
	CurrentNode string `json:"currentNode,omitempty"`

	// LastScheduleTime is when the CronJob last started a Job.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// Message is human-readable detail, for example why the CronJob is suspended.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=zpscrubcron
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.currentNode`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.status.suspended`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`

// ZfsPoolScrubCronJob schedules recurring scrubs of a ZFS pool. The operator
// maintains a Kubernetes CronJob of the same name, pinned to the node currently
// hosting the pool, and re-pins it when the pool moves.
type ZfsPoolScrubCronJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsPoolScrubCronJobSpec   `json:"spec,omitempty"`
	Status ZfsPoolScrubCronJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsPoolScrubCronJobList contains a list of ZfsPoolScrubCronJob.
type ZfsPoolScrubCronJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsPoolScrubCronJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsPoolScrubJob{}, &ZfsPoolScrubJobList{})
	SchemeBuilder.Register(&ZfsPoolScrubCronJob{}, &ZfsPoolScrubCronJobList{})
}
