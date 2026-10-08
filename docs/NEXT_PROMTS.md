but wait a minute.... we should never forget that we are not part of the k8s volume/snapshot spec. our crs are just our own management. so we even do not have to worry about group to child relation. if in our definition this is not binding anything it is simply a loose connection. and the same should hold here as for the rest. things that once were ready are never recreated. thus if a zfsgroupsnapshot is created the zfs snapshot is created for all the datasets and then the zfssnapshot resources are created. once they all are ready also the group is ready. from that point on the groups job is done and it is no longer important. the childsnapshots ake over and if someone decides to delete one than according to our definition (contradicting to the k8s definition) he should delete it. because he could do it anyways if he has access to the underlaying zfs system. 


i have an idea for the group snapshot reconciliation. We could create the snapshot on disk and then create the zfsSnapshot manifets and already set the reconciledAt status field which would prevent it from creating a snapshot if it the groupsnapshot was deleted from disk.

I also would make groupsnapshot behave identical to regualr snapshots and never recreate after reconciledAt was set once. if one dataset is missing the status should be lost. and the zfssnapshots are indipendent anyways. so deleting them is no problem. 

we should rspect the groupSnapshotId field and not create a snapshot on the original dataset if this is set but skip that part and reconcile firther. 

I also Think that we made a mistake in not reconciling the snapshot if it was once ready or lost.... i mean if the clone was lost and the @snapshot was lost but the original snapshot on the original dataset still exist we can rebuild the other structure (clone + @ restore-point snapshot) this is more robust and would help us also in handling the group snapshots as groupsnapshots could simply skip the first part (creation of snapshot at origin) completely and immediately go over to creation of the clone.


did you consider that the raw snapshot could have been promted in the meantime?