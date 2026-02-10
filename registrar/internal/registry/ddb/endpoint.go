package ddb

type DynamoDBEndpoint struct {
	// Primary key attributes
	PK string `dynamodbav:"PK"` // service#<service-name>
	SK string `dynamodbav:"SK"` // protocol#<protocol>#endpoint#<ip>

	// Core endpoint attributes
	ServiceName        string            `dynamodbav:"service_name"`
	ClusterName        string            `dynamodbav:"cluster_name"`
	IP                 string            `dynamodbav:"ip"`
	Locality           *EndpointLocality `dynamodbav:"locality"`
	AdditionalMetadata map[string]string `dynamodbav:"additional_metadata"`
	Protocol           string            `dynamodbav:"protocol,omitempty"`
	Port               uint16            `dynamodbav:"port,omitempty"`
	Weight             uint32            `dynamodbav:"weight,omitempty"`
}

type EndpointLocality struct {
	Region string `dynamodbav:"region"`
	Zone   string `dynamodbav:"zone"`
}

func (e *DynamoDBEndpoint) GetClusterName() string {
	return e.ClusterName
}

func (e *DynamoDBEndpoint) GetIp() string {
	return e.IP
}

func (e *DynamoDBEndpoint) GetAdditionalMetadata() map[string]string {
	return e.AdditionalMetadata
}

func (e *DynamoDBEndpoint) GetRegion() string {
	if e.Locality != nil {
		return e.Locality.Region
	}
	return ""
}

func (e *DynamoDBEndpoint) GetZone() string {
	if e.Locality != nil {
		return e.Locality.Zone
	}
	return ""
}
