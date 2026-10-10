generate-certs:
	@echo "Generating the throwaway upstream CA for e2e tests..."
	@mkdir -p certs
	# Generate CA private key
	@openssl genrsa -out certs/ca.key 4096
	# Generate CA certificate
	@openssl req -new -x509 -days 3650 -key certs/ca.key -out certs/ca.crt \
		-subj "/C=BR/ST=PR/L=Test/O=Test CA/CN=Test Root CA"
	@echo "CA generated in ./certs/"
