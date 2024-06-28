package queue

import "time"

var _ queueBrokerI = (*dimensionalQueueBroker)(nil)

type dimensionalQueueBroker struct {
	queues     map[QueryComponent]*queueBroker
	queueOrder []QueryComponent
	queueIndex int
}

func newDimensionalQueueBroker(
	maxTenantQueueSize int,
	additionalQueueDimensionsEnabled bool,
	forgetDelay time.Duration,
) *dimensionalQueueBroker {
	queueOrder := []QueryComponent{
		"none", // TODO: is there a constant for this?
		ingesterQueueDimension,
		storeGatewayQueueDimension,
		ingesterAndStoreGatewayQueueDimension,
	}

	queues := make(map[QueryComponent]*queueBroker, len(queueOrder))
	for _, component := range queueOrder {
		queues[component] = newQueueBroker(maxTenantQueueSize, additionalQueueDimensionsEnabled, forgetDelay)
	}

	return &dimensionalQueueBroker{
		queues:     queues,
		queueOrder: queueOrder,
	}
}

// addQuerierConnection implements queueBrokerI.
func (d *dimensionalQueueBroker) addQuerierConnection(querierID QuerierID) (resharded bool) {
	for _, queue := range d.queues {
		resharded = queue.addQuerierConnection(querierID) || resharded
	}
	return resharded
}

// dequeueRequestForQuerier implements queueBrokerI.
func (d *dimensionalQueueBroker) dequeueRequestForQuerier(lastTenantIndex int, querierID QuerierID, workerID int32) (*tenantRequest, *queueTenant, int, error) {
	// We will try at most once per queue before returning.
	tries := len(d.queues)

	var request *tenantRequest
	var tenant *queueTenant
	var tenantIndex int
	var err error

	// each worker prioritizes a different queue to start, and then cycles through the rest.
	queueIndex := int(workerID) % len(d.queueOrder)

	for i := 0; i < tries; i++ {
		queue := d.queues[d.queueOrder[queueIndex]]
		request, tenant, tenantIndex, err = queue.dequeueRequestForQuerier(lastTenantIndex, querierID, workerID)
		if tenant != nil && err == nil {
			return request, tenant, tenantIndex, nil
		}

		queueIndex = (queueIndex + 1) % len(d.queueOrder)
	}

	return request, tenant, tenantIndex, err
}

// enqueueRequestBack implements queueBrokerI.
func (d *dimensionalQueueBroker) enqueueRequestBack(request *tenantRequest, tenantMaxQueriers int) error {
	component := request.req.(*SchedulerRequest).ExpectedQueryComponentName()
	if component == "" {
		component = "none"
	}
	return d.queues[QueryComponent(component)].enqueueRequestBack(request, tenantMaxQueriers)
}

// enqueueRequestFront implements queueBrokerI.
func (d *dimensionalQueueBroker) enqueueRequestFront(request *tenantRequest, tenantMaxQueriers int) error {
	component := request.req.(*SchedulerRequest).ExpectedQueryComponentName()
	if component == "" {
		component = "none"
	}
	return d.queues[QueryComponent(component)].enqueueRequestFront(request, tenantMaxQueriers)
}

// forgetDisconnectedQueriers implements queueBrokerI.
func (d *dimensionalQueueBroker) forgetDisconnectedQueriers(now time.Time) (resharded bool) {
	for _, queue := range d.queues {
		resharded = queue.forgetDisconnectedQueriers(now) || resharded
	}
	return resharded
}

// isEmpty implements queueBrokerI.
func (d *dimensionalQueueBroker) isEmpty() (empty bool) {
	for _, queue := range d.queues {
		empty = queue.isEmpty() && empty
	}
	return empty
}

// itemCount implements queueBrokerI.
func (d *dimensionalQueueBroker) itemCount() (count int) {
	for _, queue := range d.queues {
		count += queue.itemCount()
	}
	return count
}

// notifyQuerierShutdown implements queueBrokerI.
func (d *dimensionalQueueBroker) notifyQuerierShutdown(querierID QuerierID) (resharded bool) {
	for _, queue := range d.queues {
		resharded = queue.notifyQuerierShutdown(querierID) || resharded
	}
	return resharded
}

// removeQuerierConnection implements queueBrokerI.
func (d *dimensionalQueueBroker) removeQuerierConnection(querierID QuerierID, now time.Time) (resharded bool) {
	for _, queue := range d.queues {
		resharded = queue.removeQuerierConnection(querierID, now) || resharded
	}
	return resharded
}
