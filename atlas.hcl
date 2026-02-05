// Atlas configuration for Akashi database migrations.
// See: https://atlasgo.io/atlas-schema/projects

variable "database_url" {
  type    = string
  default = getenv("DATABASE_URL")
}

// Dev database URL for diffing and linting. Atlas spins up a temporary
// schema in this database to test migrations. Point it at a disposable
// Postgres instance (e.g. the Docker dev setup).
variable "dev_url" {
  type    = string
  default = getenv("ATLAS_DEV_URL")
}

env "local" {
  src = "file://migrations"
  url = var.database_url
  dev = var.dev_url

  migration {
    dir = "file://migrations"
  }
}

env "ci" {
  src = "file://migrations"
  dev = var.dev_url

  migration {
    dir = "file://migrations"
  }
}

lint {
  // Destructive changes: DROP TABLE, DROP COLUMN, etc.
  destructive {
    error = true
  }
}
