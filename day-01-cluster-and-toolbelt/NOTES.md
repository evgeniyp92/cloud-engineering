## Useful kubectl command options

- `-o wide` - extra columns
- `-o yaml` - full object as stored
- `-o jsonpath={}` - specific value at a json field
- `-w` live updates, instead of a snapshot

## Creating a cluster with kind

`kind create cluster --name course --config kind-config.yaml`

## View current kubeconfig

`kubectl config view --minify`

## Common check commands

```
    kubectl config current-context
    kubectl config use-context kind-course
    kubectl cluster-info    
```

## Context namespace without kubens

`kubectl config set-context --current --namespace=default`

## Get pods in every namespace

`kubectl get pods -A`

## First debugging commands

```
    kubectl describe pod podlab
    kubectl get events --sort-by=.lastTimestamp
```

## Built-in Kubernetes reference

```
    kubectl explain pod.spec.containers
    kubectl explain pod.spec.containers.env.valueFrom
```

## Logs

```
    kubectl logs podlab -f
    kubectl logs podlab -c [container-name]
    kubectl logs podlab --previous
```

## Tree View in k9s

`:xray [resource type]` gives you a tree view

## Shotgun port-forwarding

```
    kubectl port-forward pod/podlab 8081:8080
    kubectl port-forward svc/[service-name] 8081:80
```

## Kind cluster commands

```
    kind get clusters
    kind get kubeconfig --name course
```

## Useful references

- Kubernetes kubectl reference - https://kubernetes.io/docs/reference/kubectl/
- kind docs - https://kind.sigs.k8s.io/
- k9s shortcuts - press `?` inside k9s

## Generate pod spec from command line

`kubectl run [image-name] --image=img --dry-run=client -o yaml > ./pod.yml`

## Make sure to run initContainers as initContainers, not as regular containers

Running initContainers as regular ones confuses K8s and marks the pod as not ready