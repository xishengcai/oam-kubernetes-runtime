# volumeTrait design

## 1. crd
```go
type VolumeTrait struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeTraitSpec   `json:"spec,omitempty"`
	Status VolumeTraitStatus `json:"status,omitempty"`
}

// A VolumeTraitSpec defines the desired state of a VolumeTrait.
type VolumeTraitSpec struct {
	VolumeList []VolumeMountItem `json:"volumeList"`
	// WorkloadReference to the workload this trait applies to.
	WorkloadReference runtimev1alpha1.TypedReference `json:"workloadRef"`
}

type VolumeMountItem struct {
	ContainerIndex int        `json:"containerIndex"`
	Paths          []PathItem `json:"paths"`
}

type PathItem struct {
	StorageClassName string             `json:"storageClassName"`
	Size             string             `json:"size"`
	Path             string             `json:"path"`
}
```

## 2. 功能目标
1. oam ContainerizedWorkload child resource 支持 动态工作负载，原先只支持deployment，现在
是在第一次初始化的工作负载的时候，发现如果添加了 volume trait，则更改工作负载为statefulSet，否则默认是deployment。

2. 用户填写信息
- 存储类名称
- 存储大小
- 容器内挂载路径
    

## 3. 实现逻辑
1. appConfig 
- set workload label: if find trait volume, set label   app.oam.dev/childResource=StatefulSet 
- apply workload

2. container workload
- apply childResource [deployment|statefulSet, service]
    if label  app.oam.dev/childResource==StatefulSet,
        apply deployment
    else
        apply statefulSet
      
2. volumeTrait
- create pvc by storageClassName and size
- patch Volumes and VolumeMount
    

