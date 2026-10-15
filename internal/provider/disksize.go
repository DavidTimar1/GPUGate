package provider

// servingContainerFloorGB and servingPersistentFloorGB are the least this program asks for once a
// model is being served: a serving image's template defaults are too small for a real model
// (docs/providers.md#both-providers).
//
// servingContainerFraction and servingPersistentFraction size the container disk and the
// persistent volume from the model's disk_bytes, with room. [OBS] runs of a 55.6 GB model used a
// 30 GB container disk and a 120 to 150 GB volume, a persistent ratio of 2.16 to 2.70;
// servingPersistentFraction sits at the low end of that range. servingContainerFraction is a
// judgement: those runs used 30 GB whatever the model size, so only the floor can be read off them.
//
// RunPod and Verda size their disks from the same figures (Verda's os_volume uses the same
// fractions and floors).
const (
	servingContainerFloorGB   = 30
	servingPersistentFloorGB  = 60
	servingContainerFraction  = 0.30
	servingPersistentFraction = 2.2
)

// ServingDiskSizes sizes a serving machine's own container disk and its persistent volume from
// the model's disk_bytes, per the fractions and floors above. diskBytes nil (the catalog carries
// no figure) falls back to the floors alone.
func ServingDiskSizes(diskBytes *int64) (containerGB, persistentGB int) {
	weightsGB := 0.0
	if diskBytes != nil {
		weightsGB = float64(*diskBytes) / 1_000_000_000
	}
	containerGB = int(weightsGB*servingContainerFraction) + 1
	if containerGB < servingContainerFloorGB {
		containerGB = servingContainerFloorGB
	}
	persistentGB = int(weightsGB*servingPersistentFraction) + 1
	if persistentGB < servingPersistentFloorGB {
		persistentGB = servingPersistentFloorGB
	}
	return containerGB, persistentGB
}
