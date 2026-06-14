# Co-op Eval Fixture: Hive Marketplace

This workspace is a pinned copy of `codebasics/hive-marketplace` with a small
eval overlay.

Use the existing marketplace flow:

- Backend: `backend/app`, FastAPI, SQLAlchemy, SQLite.
- Frontend: `frontend/app`, Next.js app router.
- Existing payment seam: `backend/app/services/payment_service.py` and
  `backend/app/routers/orders.py`.
- Existing buyer flow: cart, buy now, orders.
- Existing seller flow: products, seller orders, dashboard.

For one-time payments, add Stripe Checkout to the app's checkout/order path and
handle `checkout.session.completed` in a signed webhook endpoint. Do not create a
separate demo server.

For Connect marketplace work, attach sellers to connected account IDs and route
payments through a supported Connect flow. Do not invent split payments for a
single Checkout Session with multiple destinations.

Eval verification should exercise the app over HTTP and inspect persisted app
state, in addition to any Stripe API assertions.
