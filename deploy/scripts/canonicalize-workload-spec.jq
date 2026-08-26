def require_object($path):
  if type == "object" then .
  else error($path + " must be an object")
  end;

def optional_object($path):
  if . == null or type == "object" then .
  else error($path + " must be an object or null")
  end;

def optional_array($path):
  if . == null or type == "array" then .
  else error($path + " must be an array or null")
  end;

. | require_object("resource") as $resource
| if $resource.kind != $kind then
    error("resource kind must be " + $kind)
  else
    $resource.spec | require_object(".spec") as $spec
    | if $kind == "Job" then
        $spec
      else
        $resource.metadata | require_object(".metadata") as $metadata
        | $metadata.annotations | optional_object(".metadata.annotations") as $annotations
        | {
            metadata: {
              annotations: (
                if $kind == "Deployment" then
                  (($annotations // {}) | del(."deployment.kubernetes.io/revision"))
                else
                  ($annotations // {})
                end
              )
            },
            spec: $spec
          }
      end
  end
| if $kind != "Job" then
    .
  else
    . as $spec
    | $spec.template | require_object(".spec.template") as $template
    | $template.metadata | require_object(".spec.template.metadata") as $metadata
    | $template.spec | require_object(".spec.template.spec") as $pod_spec
    | $metadata.annotations | optional_object(".spec.template.metadata.annotations") as $annotations
    | $metadata.labels | optional_object(".spec.template.metadata.labels") as $labels
    | $metadata.finalizers | optional_array(".spec.template.metadata.finalizers") as $finalizers
    | {
        templateSpec: $pod_spec,
        templateMetadata: {
          annotations: $annotations,
          labels: (($labels // {}) | del(
            ."batch.kubernetes.io/controller-uid",
            ."batch.kubernetes.io/job-name",
            ."controller-uid",
            ."job-name"
          )),
          name: $metadata.name,
          generateName: $metadata.generateName,
          finalizers: $finalizers
        },
        parallelism: $spec.parallelism,
        completions: $spec.completions,
        activeDeadlineSeconds: $spec.activeDeadlineSeconds,
        backoffLimit: $spec.backoffLimit,
        backoffLimitPerIndex: $spec.backoffLimitPerIndex,
        maxFailedIndexes: $spec.maxFailedIndexes,
        ttlSecondsAfterFinished: $spec.ttlSecondsAfterFinished,
        completionMode: $spec.completionMode,
        suspend: $spec.suspend,
        manualSelector: $spec.manualSelector,
        selector: (if $spec.manualSelector then $spec.selector else null end),
        podFailurePolicy: $spec.podFailurePolicy,
        successPolicy: $spec.successPolicy,
        podReplacementPolicy: $spec.podReplacementPolicy,
        managedBy: $spec.managedBy
      }
  end
