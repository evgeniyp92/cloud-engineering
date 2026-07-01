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

## Annotate pods with deployment version

kubectl annotate <thing> <thing-name> kubernetes.io/change-cause="initial 
deploy, VERSION 1.0.0"

## Making and undoing rollouts

```sh
kubectl rollout history deployment/podlab
kubectl rollout history deployment/podlab --revision=2   # full template of a revision
```

Shipping a bad release and then undoing it

```sh
kubectl set env deployment/podlab VERSION=2.1.0-broken
kubectl annotate deployment podlab kubernetes.io/change-cause="VERSION 2.1.0-broken" --overwrite
kubectl rollout undo deployment/podlab                    # back to previous (2.0.0)
kubectl rollout undo deployment/podlab --to-revision=1    # explicitly back to 1.0.0
kubectl rollout history deployment/podlab
```

Note in the history: rolling back doesn't go "backwards" — revision 1's template is re-released as a **new** highest revision, and the watcher column flips accordingly. Roll forward to `2.0.0` again before continuing (`kubectl set env deployment/podlab VERSION=2.0.0`).

## Use `rollout pause` to suspend immediate change detection and application for bigger changes, and `rollout resume` once done.

## When updating image names in deployments the syntax is `old_image=new_image:tag`

## Important imperative commands

`kubectl set image deploy/x ctr=img:v1`

`kubectl scale`

`kubectl rollout undo`

`kubectl rollout status`

kubectl set image needs the container name — get it with kubectl get deploy x -o jsonpath='{.spec.template.spec.containers[*].name}'.

