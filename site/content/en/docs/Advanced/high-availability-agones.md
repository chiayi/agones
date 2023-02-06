---
title: "High Availability Agones"
date: 2023-02-08T15:02:23Z
weight: 20
description: >
  Allows controllers to be strictly for control loops; feature separates extensions and webhooks out of your controllers
---

## High Availability for Agones Controller

The `agones-controller` pod is currently not only used for control loops of game server related resources, but also include other services such as webhooks and allocation extension srvices. High availability agones mean as opposed to having controller host everything mentioned, we will add additional `extensions` pods that exposes the webhook and allocations service. Thus, making `agones-controller` pod strictly for control loops. Read more about pod disruption [here](https://agones.dev/site/docs/advanced/controlling-disruption/)

{{< alpha title="`split controller and extensions` API" gate="SplitControllerAndExtensions" >}}

{{< alert title="GKE Autopilot Clusters" color="warning" >}}
In order to use GKE Autopilot clusters with Agones, this feature flag `SplitControllerAndExtensions` must be enabled along with `SafeToEvict` (should already be enabled). 

More info on `SafeToEvict` [here](https://agones.dev/site/docs/advanced/controlling-disruption/#disruption-in-kubernetes)
{{< /alert >}}

## Extension Pod Configrations 

Extensions pod values are currently inherited through the `controller` values within the `values.yaml` through yaml inheritance. This means that changes made to the `controller` configurations within the file will have a direct effect on the `extensions` values. Essentially acts as a copy & paste. To override the configurations within `values.yaml`, declare and initialize the desired key and value within the `extensions` section within `values.yaml`. For example, lets change the `apiServerQPS` field value inhertied for `extensions` to 600 instead of the original 400:  

```
controller: &example
    numWorkers: 100
    apiServerQPS: 400
    apiServerQPSBurst: 500
    http:
      port: 8080
...
  extensions:
    <<: *example
    apiServerQPS: 600
```

Another important note is that `helm --set` sets the variable after the yaml inheritance. Changing `controller` value through this method will not change the values that `extensions` inherited. The same applies for when `--values` or `-f` is used. 

To change `controller.numWorkers` to 200 from 100 values and through the use of `helm --set`, add the follow to the `helm` command:

{{< alert color="warning" >}} Important: This will not have any effect on any `extensions` values! {{< /alert >}}
```
 ...
 --set agones.controller.numWorkers=200
 ...
```

## Feature Enabled 

The `SplitControllerAndExtensions` feature creates sperate pods for extensions, currently the replication is set to `2` but it can definitely be changed through `values.yaml` or `helm --set`. Extensions pods do have PDB (PodDisruptionBudget) and has the `minAvailable` set to `1`.

When feature is enabled, expect more resources to be used due to having more pods. The pods list should look something like this:

```
NAME                                 READY   STATUS    RESTARTS   AGE
agones-allocator-78c6b8c79-h9nqc     1/1     Running   0          23h
agones-allocator-78c6b8c79-l2bzp     1/1     Running   0          23h
agones-allocator-78c6b8c79-rw75j     1/1     Running   0          23h
agones-controller-fbf944f4-vs9xx     1/1     Running   0          23h
agones-extensions-5648fc7dcf-hm6lk   1/1     Running   0          23h
agones-extensions-5648fc7dcf-qbc6h   1/1     Running   0          23h
agones-ping-5b9647874-2rrl6          1/1     Running   0          27h
agones-ping-5b9647874-rksgg          1/1     Running   0          27h
```

## Feature Design

[HA Agones design](https://github.com/googleforgames/agones/issues/2797)

The design oes through the different phases planned for HA Agones as well as other approaches that were considered.