## cf-operator util instance-group

Resolves instance group properties of a BOSH manifest

### Synopsis

Resolves instance group properties of a BOSH manifest.

This will resolve the properties of an instance group and return a manifest for that instance group.
Also calculates and prints the BPM configurations for all BOSH jobs of that instance group.



```
cf-operator util instance-group [flags]
```

### Options

```
  -b, --base-dir string              (BASE_DIR) a path to the base directory
  -m, --bosh-manifest-path string    (BOSH_MANIFEST_PATH) path to the bosh manifest file
  -n, --deployment-name string       (DEPLOYMENT_NAME) name of the bdpl resource
  -h, --help                         help for instance-group
      --initial-rollout              (INITIAL_ROLLOUT) Initial rollout of bosh deployment. (default true)
  -g, --instance-group-name string   (INSTANCE_GROUP_NAME) name of the instance group for data gathering
      --output-file-path string      (OUTPUT_FILE_PATH) Path of the file to which json output is written.
```

### SEE ALSO

* [cf-operator util](cf-operator_util.md)	 - Calls a utility subcommand

###### Auto generated by spf13/cobra on 20-Mar-2020
