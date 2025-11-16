import { NextRequest, NextResponse } from "next/server";
import { getTodoById, updateTodo, deleteTodo } from "@/lib/db";

// GET /api/todos/:id - Get todo by ID
export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await params;
    const todoId = parseInt(id, 10);

    if (isNaN(todoId)) {
      return NextResponse.json(
        { error: "Invalid todo ID" },
        { status: 400 }
      );
    }

    const todo = getTodoById(todoId);

    if (!todo) {
      return NextResponse.json(
        { error: "Todo not found" },
        { status: 404 }
      );
    }

    return NextResponse.json(todo);
  } catch (error) {
    console.error("Error fetching todo:", error);
    return NextResponse.json(
      { error: "Failed to fetch todo" },
      { status: 500 }
    );
  }
}

// PATCH /api/todos/:id - Update todo
export async function PATCH(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await params;
    const todoId = parseInt(id, 10);

    if (isNaN(todoId)) {
      return NextResponse.json(
        { error: "Invalid todo ID" },
        { status: 400 }
      );
    }

    const body = await request.json();
    const updates: { title?: string; completed?: boolean } = {};

    if (body.title !== undefined) {
      if (typeof body.title !== "string") {
        return NextResponse.json(
          { error: "Title must be a string" },
          { status: 400 }
        );
      }
      updates.title = body.title.trim();
    }

    if (body.completed !== undefined) {
      if (typeof body.completed !== "boolean") {
        return NextResponse.json(
          { error: "Completed must be a boolean" },
          { status: 400 }
        );
      }
      updates.completed = body.completed;
    }

    const todo = updateTodo(todoId, updates);

    if (!todo) {
      return NextResponse.json(
        { error: "Todo not found" },
        { status: 404 }
      );
    }

    return NextResponse.json(todo);
  } catch (error) {
    console.error("Error updating todo:", error);
    return NextResponse.json(
      { error: "Failed to update todo" },
      { status: 500 }
    );
  }
}

// DELETE /api/todos/:id - Delete todo
export async function DELETE(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await params;
    const todoId = parseInt(id, 10);

    if (isNaN(todoId)) {
      return NextResponse.json(
        { error: "Invalid todo ID" },
        { status: 400 }
      );
    }

    const success = deleteTodo(todoId);

    if (!success) {
      return NextResponse.json(
        { error: "Todo not found" },
        { status: 404 }
      );
    }

    return NextResponse.json({ success: true });
  } catch (error) {
    console.error("Error deleting todo:", error);
    return NextResponse.json(
      { error: "Failed to delete todo" },
      { status: 500 }
    );
  }
}
