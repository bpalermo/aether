generate-certs:
	@echo "Generating CA and TLS certificates for e2e tests..."
	@mkdir -p certs
	# Generate CA private key
	@openssl genrsa -out certs/ca.key 4096
	# Generate CA certificate
	@openssl req -new -x509 -days 3650 -key certs/ca.key -out certs/ca.crt \
		-subj "/C=BR/ST=PR/L=Test/O=Test CA/CN=Test Root CA"
	# Generate svc-0 private key
	@openssl genrsa -out certs/svc-0.key 4096
	# Generate svc-0 certificate request
	@openssl req -new -key certs/svc-0.key -out certs/svc-0.csr \
		-subj "/C=BR/ST=PR/L=Test/O=Test/CN=svc-0"
	# Sign svc-0 certificate with CA
	@openssl x509 -req -days 365 -in certs/svc-0.csr \
		-CA certs/ca.crt -CAkey certs/ca.key \
		-CAcreateserial -out certs/svc-0.crt
	# Generate svc-1 private key
	@openssl genrsa -out certs/svc-1.key 4096
	# Generate svc-1 certificate request
	@openssl req -new -key certs/svc-1.key -out certs/svc-1.csr \
		-subj "/C=BR/ST=PR/L=Test/O=Test/CN=svc-1"
	# Sign svc-1 certificate with CA
	@openssl x509 -req -days 365 -in certs/svc-1.csr \
		-CA certs/ca.crt -CAkey certs/ca.key \
		-CAcreateserial -out certs/svc-1.crt
	# Generate svc-2 private key
	@openssl genrsa -out certs/svc-2.key 4096
	# Generate svc-2 certificate request
	@openssl req -new -key certs/svc-2.key -out certs/svc-2.csr \
		-subj "/C=BR/ST=PR/L=Test/O=Test/CN=svc-2"
	# Sign svc-2 certificate with CA
	@openssl x509 -req -days 365 -in certs/svc-2.csr \
		-CA certs/ca.crt -CAkey certs/ca.key \
		-CAcreateserial -out certs/svc-2.crt
	# Generate svc-3 private key
	@openssl genrsa -out certs/svc-3.key 4096
	# Generate svc-3 certificate request
	@openssl req -new -key certs/svc-3.key -out certs/svc-3.csr \
		-subj "/C=BR/ST=PR/L=Test/O=Test/CN=svc-3"
	# Sign svc-3 certificate with CA
	@openssl x509 -req -days 365 -in certs/svc-3.csr \
		-CA certs/ca.crt -CAkey certs/ca.key \
		-CAcreateserial -out certs/svc-3.crt
	# Clean up CSR
	@rm -f certs/svc-0.csr certs/svc-1.csr certs/svc-2.csr certs/svc-3.csr certs/ca.srl
	# Give read to other so envoy can read it
	@chmod 644 certs/svc-0.key certs/svc-1.key certs/svc-2.key certs/svc-3.key
	@echo "Certificates generated in ./certs/"
