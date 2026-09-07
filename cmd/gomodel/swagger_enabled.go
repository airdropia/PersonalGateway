//go:build swagger

package main

import swaggerdocs "github.com/airdropia/pgw/cmd/gomodel/docs"

func configureSwaggerDocs(basePath string) {
	swaggerdocs.SwaggerInfo.BasePath = basePath
}
