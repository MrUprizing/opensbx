package docs

import "testing"

func TestSwaggerMetadataIsRegistered(t *testing.T) {
	if SwaggerInfo == nil {
		t.Fatal("SwaggerInfo is nil")
	}
	if SwaggerInfo.Title != "Opensbx API" || SwaggerInfo.Version != "1.0" || SwaggerInfo.BasePath != "/v1" || SwaggerInfo.Host != "localhost:18089" {
		t.Fatalf("unexpected Swagger metadata: title=%q version=%q host=%q basePath=%q", SwaggerInfo.Title, SwaggerInfo.Version, SwaggerInfo.Host, SwaggerInfo.BasePath)
	}
}
