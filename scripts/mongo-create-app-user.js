// Run this while authenticated as a MongoDB administrator:
// mongosh "mongodb://ADMIN@127.0.0.1:27017/admin?authSource=admin" \
//   --file scripts/mongo-create-app-user.js
//
// The password is read interactively and is never stored in this file.

const applicationDatabase = db.getSiblingDB("seo_monitor");
const applicationUsername = "seo_monitor_app";
const applicationPassword = passwordPrompt();

if (!applicationPassword || applicationPassword.length < 20) {
  throw new Error("application password must contain at least 20 characters");
}

const existingUser = applicationDatabase.getUser(applicationUsername);
if (existingUser) {
  applicationDatabase.updateUser(applicationUsername, {
    pwd: applicationPassword,
    roles: [{ role: "readWrite", db: "seo_monitor" }],
  });
  print("Updated MongoDB application user: " + applicationUsername);
} else {
  applicationDatabase.createUser({
    user: applicationUsername,
    pwd: applicationPassword,
    roles: [{ role: "readWrite", db: "seo_monitor" }],
  });
  print("Created MongoDB application user: " + applicationUsername);
}
