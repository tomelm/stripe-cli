# Co-op Eval Fixture: Easy!Appointments

This workspace is a pinned copy of `alextselegidis/easyappointments` with a
small eval overlay.

Use the existing scheduling app shape:

- Controllers and routes live under `application/controllers`.
- Provider, customer, service, and appointment behavior already exists.
- Docker Compose starts PHP-FPM, nginx, MySQL, and supporting dev services.

For Connect platform work, providers should own connected account IDs and
appointments should move through a paid state after Stripe webhook confirmation.
Do not implement payment as an isolated page that bypasses the appointment model.

Eval verification should create or seed a provider, customer, service, and
appointment, then prove payment state changes through the app.
