package controlplane

import "time"

const (
	NodeHeartbeatTTL     = 20 * time.Second
	InstanceHeartbeatTTL = 20 * time.Second
)

func normalizeMembershipLiveness(nodes map[string]Node, instances map[string]AgentInstance, now time.Time) {
	for id, node := range nodes {
		if nodeHeartbeatExpired(node, now) {
			node.State = NodeStateUnavailable
			nodes[id] = node
		}
	}

	for id, instance := range instances {
		node, hasNode := nodes[instance.NodeID]
		if instanceHeartbeatExpired(instance, node, hasNode, now) {
			instance.State = InstanceStateUnavailable
			instances[id] = instance
		}
	}
}

func nodeHeartbeatExpired(node Node, now time.Time) bool {
	if node.State == NodeStateUnavailable {
		return false
	}
	if node.LastHeartbeatAt.IsZero() {
		return false
	}
	return now.Sub(node.LastHeartbeatAt) > NodeHeartbeatTTL
}

func instanceHeartbeatExpired(instance AgentInstance, node Node, hasNode bool, now time.Time) bool {
	if instance.State == InstanceStateUnavailable {
		return false
	}
	if hasNode && node.State == NodeStateUnavailable {
		return true
	}
	if instance.LastHeartbeatAt.IsZero() {
		return false
	}
	return now.Sub(instance.LastHeartbeatAt) > InstanceHeartbeatTTL
}
